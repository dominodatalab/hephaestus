package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// normalizeRegistryHost reduces a docker config server key or an image ref
// domain to a plain hostname so the two can be compared. Config keys may carry
// a scheme, a path and even userinfo; hosts are compared case-insensitively.
// Docker Hub goes by several names, so they all collapse to "docker.io".
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

// usedRegistryHosts returns the set of registry hostnames the build is known to
// use, taken from the refs it pushes and the remote caches it imports. Base
// images are not included: they live in the Dockerfile, and resolving them means
// parsing it, so buildkit is left to fail the build if a base image cred is bad.
//
// A nil result means "unknown, check everything". A ref that does not parse
// could name any registry, so narrowing the check on it would be unsound.
func usedRegistryHosts(refs []string) map[string]bool {
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

	usedHosts := usedRegistryHosts(refs)
	if usedHosts == nil {
		logger.Info("Checking every credential: a build reference could not be parsed", "references", refs)
	}

	var errs []error
	for server, auth := range configJSON.Auths {
		host := normalizeRegistryHost(server)

		// Only check credentials for registries this build is known to use. A
		// cred for any other registry cannot fail the build here, no matter its
		// state. With no known hosts, check everything.
		if len(usedHosts) > 0 && !usedHosts[host] {
			logger.Info("Skipping credential check for registry not used by this build", "registry", host)
			continue
		}

		auth.ServerAddress = server

		var authErr error
		err := wait.ExponentialBackoffWithContext(ctx, defaultBackoff, func(ctx context.Context) (bool, error) {
			if _, _, authErr = svc.Auth(ctx, &auth, "DominoDataLab_Hephaestus/1.0"); authErr != nil {
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

		// A cancelled or expired build context is fatal. It is not a registry
		// being unreachable, so do not skip it, and do not discard credential
		// failures already proven for other registries.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return multierr.Append(multierr.Combine(errs...), err)
		}

		// A registry we could not reach at all must not fail the build: it may be
		// down for reasons that have nothing to do with these creds, and buildkit
		// still fails the build later if it truly needs this registry. Anything
		// the registry did answer, 401 and 403 alike, is a credential problem, so
		// fail now while the error is clear and no worker time has been spent.
		if _, ok := errors.AsType[net.Error](authErr); ok {
			logger.Info("Skipping credential check for unreachable registry", "registry", host, "reason", authErr.Error())
			continue
		}

		reason := err
		if authErr != nil {
			reason = authErr
		}
		//nolint:lll
		detailedErr := fmt.Errorf("client credentials are invalid for registry %q.\nMake sure the following sources of credentials are correct: %s.\nUnderlying error: %w", host, strings.Join(helpMessage, ", "), reason)
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
