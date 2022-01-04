package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"

	hephapi "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type AuthConfigs map[string]AuthConfig

type DockerConfig struct {
	Auths AuthConfigs `json:"auths"`
}

func Extract(host string, data []byte) (string, string, error) {
	var conf DockerConfig
	if err := json.Unmarshal(data, &conf); err != nil {
		return "", "", nil
	}

	ac, ok := conf.Auths[host]
	if !ok {
		var servers []string
		for url := range conf.Auths {
			servers = append(servers, url)
		}

		return "", "", fmt.Errorf("registry %q is not in server list %v", host, servers)
	}

	return ac.Username, ac.Password, nil
}

func Persist(ctx context.Context, cfg *rest.Config, credentials []hephapi.RegistryCredentials) (string, error) {
	dir, err := os.MkdirTemp("", "docker-config-")
	if err != nil {
		return "", err
	}

	auths := AuthConfigs{}
	for _, cred := range credentials {
		switch {
		case cred.BasicAuth != nil:
			auths[cred.Server] = AuthConfig{
				Username: cred.BasicAuth.Username,
				Password: cred.BasicAuth.Password,
			}
		case cred.Secret != nil:
			clientset, err := kubernetes.NewForConfig(cfg)
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

			username, password, err := Extract(cred.Server, secret.Data[corev1.DockerConfigJsonKey])
			if err != nil {
				return "", err
			}
			auths[cred.Server] = AuthConfig{
				Username: username,
				Password: password,
			}
		case pointer.BoolDeref(cred.CloudProvided, false):
			panic("cloud auth credentials not supported")
		}
	}

	dockerCfg := DockerConfig{Auths: auths}

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
