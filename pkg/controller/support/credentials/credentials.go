package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	typesregistry "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/errdefs"
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
			pac, err := CloudAuthRegistry.RetrieveAuthorization(ctx, logger, cred.Server)
			if err != nil {
				if err != cloudauth.ErrNoLoader {
					return "", nil, fmt.Errorf("registry authorization failed: %w", err)
				}
				return "", nil, fmt.Errorf("failed to authorize server %s, credentials may be misconfigured", cred.Server)
			}

			ac = *pac
			helpMessage = append(helpMessage, fmt.Sprintf("cloud provider access configuration (server: %s)", cred.Server))
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

func Verify(ctx context.Context, configDir string, insecureRegistries []string, helpMessage []string) error {
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

	var errs []error
	for server, auth := range configJSON.Auths {
		auth := auth
		auth.ServerAddress = server

		err := wait.ExponentialBackoffWithContext(ctx, defaultBackoff, func(ctx context.Context) (bool, error) {
			if _, _, err = svc.Auth(ctx, &auth, "DominoDataLab_Hephaestus/1.0"); err != nil {
				if errdefs.IsUnauthorized(err) {
					return false, err
				}
				return false, nil
			}

			return true, nil
		})

		if err != nil {
			//nolint:lll
			detailedErr := fmt.Errorf("client credentials are invalid for registry %q.\nMake sure the following sources of credentials are correct: %s.\nUnderlying error: %w", server, strings.Join(helpMessage, ", "), err)
			errs = append(errs, detailedErr)
		}
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
