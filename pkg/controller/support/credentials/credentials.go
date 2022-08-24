package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/registry"
	"github.com/go-logr/logr"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"

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
type AuthConfigs map[string]types.AuthConfig

// DockerConfigJSON models the structure of .dockerconfigjson data.
type DockerConfigJSON struct {
	Auths AuthConfigs `json:"auths"`
}

func Persist(ctx context.Context, cfg *rest.Config, credentials []hephv1.RegistryCredentials) (string, error) {
	dir, err := os.MkdirTemp("", "docker-config-")
	if err != nil {
		return "", err
	}

	auths := AuthConfigs{}
	for _, cred := range credentials {
		var ac types.AuthConfig

		switch {
		case cred.Secret != nil:
			clientset, err := clientsetFunc(cfg)
			if err != nil {
				return "", err
			}
			client := clientset.CoreV1().Secrets(cred.Secret.Namespace)

			secret, err := client.Get(ctx, cred.Secret.Name, metav1.GetOptions{})
			if err != nil {
				return "", err
			}

			if secret.Type != corev1.SecretTypeDockerConfigJson {
				return "", fmt.Errorf("invalid secret")
			}

			var conf DockerConfigJSON
			if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &conf); err != nil {
				return "", err
			}

			for server, config := range conf.Auths {
				auths[server] = config
			}

			continue
		case cred.BasicAuth != nil:
			ac = types.AuthConfig{
				Username: cred.BasicAuth.Username,
				Password: cred.BasicAuth.Password,
			}
		case pointer.BoolDeref(cred.CloudProvided, false):
			pac, err := CloudAuthRegistry.RetrieveAuthorization(ctx, cred.Server)
			if err != nil {
				return "", err
			}

			ac = *pac
		default:
			return "", fmt.Errorf("credential %v is missing auth section", cred)
		}

		auths[cred.Server] = ac
	}
	dockerCfg := DockerConfigJSON{Auths: auths}

	configJSON, err := json.Marshal(dockerCfg)
	if err != nil {
		return "", err
	}

	filename := filepath.Join(dir, "config.json")
	if err = os.WriteFile(filename, configJSON, 0644); err != nil {
		return "", err
	}

	return dir, err
}

func Verify(ctx context.Context, configDir string, insecureRegistries []string) error {
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

		if _, _, err = svc.Auth(ctx, &auth, "DominoDataLab_Hephaestus/1.0"); err != nil {
			errs = append(errs, fmt.Errorf("%q client credentials are invalid: %w", server, err))
		}
	}
	if len(errs) != 0 {
		return multierr.Combine(errs...)
	}

	return nil
}

// LoadCloudProviders adds all cloud authentication providers to the CloudAuthRegistry.
func LoadCloudProviders(log logr.Logger) error {
	if err := acr.Register(log, CloudAuthRegistry); err != nil {
		return fmt.Errorf("ACR registration failed: %w", err)
	}
	if err := ecr.Register(log, CloudAuthRegistry); err != nil {
		return fmt.Errorf("ECR registration failed: %w", err)
	}
	if err := gcr.Register(log, CloudAuthRegistry); err != nil {
		return fmt.Errorf("GCR registration failed: %w", err)
	}

	return nil
}
