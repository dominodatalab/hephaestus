package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/errdefs"

	"github.com/distribution/reference"
	typesregistry "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/registry"
	"github.com/go-logr/logr"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/acr"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/ecr"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/gcr"
)

var CloudAuthRegistry = &cloudauth.Registry{}

var clientsetFunc = func(config *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(config)
}

// AuthConfigs is a map of registry urls to authentication credentials.
type AuthConfigs map[string]typesregistry.AuthConfig

// DockerConfigJSON models the structure of .dockerconfigjson data.
type DockerConfigJSON struct {
	Auths AuthConfigs `json:"auths"`
}

var defaultBackoff = wait.Backoff{ // retries after 1s 2s 4s 8s 16s
	Duration: time.Second,
	Factor:   2,
	Steps:    6,
}

func Persist(
	ctx context.Context,
	logger logr.Logger,
	cfg *rest.Config,
	credentials []hephv1.RegistryCredentials,
) (string, []string, error) {
	dir, err := os.MkdirTemp("", "docker-config-")
	if err != nil {
		return "", nil, err
	}

	auths := AuthConfigs{}
	// as we can't establish a 1:1 correlation between the server field
	// and the computed docker config.json in downstream authentication
	// helpMessage stores general meta-information about the creds
	// in use that can be supplied to any error message(s) that surface
	// for more easily debugging the source of a failed auth.
	var helpMessage []string
	for _, cred := range credentials {
		var ac typesregistry.AuthConfig

		switch {
		case cred.Secret != nil:
			clientset, err := clientsetFunc(cfg)
			if err != nil {
				return "", nil, err
			}
			client := clientset.CoreV1().Secrets(cred.Secret.Namespace)

			secret, err := client.Get(ctx, cred.Secret.Name, metav1.GetOptions{})
			if err != nil {
				return "", nil, err
			}

			if secret.Type != corev1.SecretTypeDockerConfigJson {
				return "", nil, fmt.Errorf("invalid secret")
			}

			var conf DockerConfigJSON
			if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &conf); err != nil {
				return "", nil, err
			}

			var servers []string
			for server, config := range conf.Auths {
				auths[server] = config
				servers = append(servers, server)
			}

			//nolint:lll
			helpMessage = append(helpMessage, fmt.Sprintf("secret %q in namespace %q (credentials for servers: %s)", cred.Secret.Name, cred.Secret.Namespace, strings.Join(servers, ", ")))
			continue
		case cred.BasicAuth != nil:
			ac = typesregistry.AuthConfig{
				Username: cred.BasicAuth.Username,
				Password: cred.BasicAuth.Password,
			}

			helpMessage = append(helpMessage, "basic authentication username and password")
		default:
			// Cloud auth credentials are fetched on-demand by RefreshingAuthProvider.
			// Skip writing them to static config to allow on-demand refresh for long builds.
			logger.Info("Cloud registry will use on-demand authentication", "server", cred.Server)
			helpMessage = append(helpMessage, fmt.Sprintf("cloud provider on-demand authentication (server: %s)", cred.Server))
			continue
		}

		auths[cred.Server] = ac
	}
	dockerCfg := DockerConfigJSON{Auths: auths}

	configJSON, err := json.Marshal(dockerCfg)
	if err != nil {
		return "", nil, err
	}

	filename := filepath.Join(dir, "config.json")
	if err = os.WriteFile(filename, configJSON, 0644); err != nil {
		return "", nil, err
	}

	return dir, helpMessage, err
}

// normalizeRegistryHost reduces a docker config server key or a ref domain to a
// bare host so the two compare. Keys can carry a scheme, a path and userinfo.
// Docker Hub answers to several names, so they all collapse to docker.io.
func normalizeRegistryHost(server string) string {
	host := strings.ToLower(registry.ConvertToHostname(server))
	if at := strings.LastIndex(host, "@"); at != -1 {
		host = host[at+1:]
	}

	if host == "index.docker.io" || host == "registry-1.docker.io" {
		host = "docker.io"
	}

	return host
}

// targetedRegistryHosts returns the hosts a build pushes to or pulls cache from.
// Narrower than "hosts the build uses": a base image pull is a use and is not
// included, because base images live in the Dockerfile and we never parse it.
// Buildkit authenticates those itself.
//
// nil or empty both mean "check everything": a ref we cannot parse could name
// any host, and no refs at all tells us nothing.
func targetedRegistryHosts(refs []string) map[string]bool {
	hosts := map[string]bool{}
	for _, ref := range refs {
		named, err := reference.ParseNormalizedNamed(ref)
		if err != nil {
			return nil
		}

		hosts[normalizeRegistryHost(reference.Domain(named))] = true
	}

	return hosts
}

// unreachable reports whether the registry never answered, rather than answering
// with a rejection.
//
// Don't test the top-level error's type. http.Client wraps every failure in
// *url.Error, which satisfies net.Error whatever the cause, and docker's bearer
// transport returns registry status errors through it. Gating on that reads a
// bearer 401 as an outage and lets a typo'd password reach a leased worker.
func unreachable(err error) bool {
	if err == nil {
		return false
	}

	// The registry named the credential as the problem, so it answered.
	if errdefs.IsUnauthorized(err) {
		return false
	}

	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}

	_, ok := errors.AsType[net.Error](err)

	return ok
}

func Verify(
	ctx context.Context,
	logger logr.Logger,
	configDir string,
	insecureRegistries []string,
	helpMessage []string,
	refs []string,
) error {
	filename := filepath.Join(configDir, "config.json")
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	configJSON := DockerConfigJSON{}
	if err = json.Unmarshal(data, &configJSON); err != nil {
		return err
	}

	svc, err := registry.NewService(registry.ServiceOptions{InsecureRegistries: insecureRegistries})
	if err != nil {
		return err
	}

	targetedHosts := targetedRegistryHosts(refs)
	if targetedHosts == nil {
		logger.Info("Checking every credential: a build reference could not be parsed", "references", refs)
	}

	var errs []error
	for server, auth := range configJSON.Auths {
		host := normalizeRegistryHost(server)

		// A cred for any other registry cannot fail the build, whatever state it
		// is in. Base image registries land here too.
		if len(targetedHosts) > 0 && !targetedHosts[host] {
			logger.Info("Skipping credential check: registry is not a build target or cache ref", "registry", host)
			continue
		}

		auth.ServerAddress = server

		var authErr, answered error
		err := wait.ExponentialBackoffWithContext(ctx, defaultBackoff, func(ctx context.Context) (bool, error) {
			if _, _, authErr = svc.Auth(ctx, &auth, "DominoDataLab_Hephaestus/1.0"); authErr != nil {
				// Remember the first answer we got. Only the last retry survives in
				// authErr, so a registry that answers and then goes down would
				// otherwise look unreachable and let the bad cred through. Docker's
				// own HTTPS-then-HTTP attempts inside one Auth call keep only their
				// last error, so that layer can still mask an answer.
				if answered == nil && !unreachable(authErr) {
					answered = authErr
				}
				if errdefs.IsUnauthorized(authErr) {
					return false, authErr
				}
				return false, nil
			}

			return true, nil
		})
		if err == nil {
			continue
		}

		// A dead build context is fatal. It is not an unreachable registry, and it
		// must not discard cred failures already found, this registry's included.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return multierr.Combine(append(errs, answered, err)...)
		}

		// A registry that never answered must not fail the build. It may be down
		// for reasons unrelated to these creds, and buildkit fails later if the
		// build really needs it. Anything it did answer, 401 and 403 alike, is a
		// cred problem, so fail now before any worker time is spent.
		if answered == nil && unreachable(authErr) {
			logger.Info("Skipping credential check for unreachable registry", "registry", host, "reason", authErr.Error())
			continue
		}

		reason := err
		switch {
		case answered != nil:
			reason = answered
		case authErr != nil:
			reason = authErr
		}
		//nolint:lll
		detailedErr := fmt.Errorf("client credentials are invalid for registry %q.\nMake sure the following sources of credentials are correct: %s.\nUnderlying error: %w", server, strings.Join(helpMessage, ", "), reason)
		errs = append(errs, detailedErr)
	}
	if len(errs) != 0 {
		return multierr.Combine(errs...)
	}

	return nil
}

// LoadCloudProviders adds all cloud authentication providers to the CloudAuthRegistry.
func LoadCloudProviders(ctx context.Context, log logr.Logger) error {
	if err := acr.Register(ctx, log, CloudAuthRegistry); err != nil {
		return fmt.Errorf("ACR registration failed: %w", err)
	}
	if err := ecr.Register(ctx, log, CloudAuthRegistry); err != nil {
		return fmt.Errorf("ECR registration failed: %w", err)
	}
	if err := gcr.Register(ctx, log, CloudAuthRegistry); err != nil {
		return fmt.Errorf("GCR registration failed: %w", err)
	}

	return nil
}
