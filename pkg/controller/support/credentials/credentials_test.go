package credentials

import (
	"context"
	"encoding/json"
	cfg "github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/logger"
	"github.com/go-logr/zapr"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

func TestPersist(t *testing.T) {
	t.Run("all_secret_auths", func(t *testing.T) {
		config := DockerConfigJSON{
			Auths: AuthConfigs{
				"registry1.com": types.AuthConfig{
					Username: "happy",
					Password: "gilmore",
				},
				"registry2.com": types.AuthConfig{
					Username: "billy",
					Password: "madison",
				},
			},
		}
		expected, err := json.Marshal(config)
		require.NoError(t, err)

		clientsetFunc = func(*rest.Config) (kubernetes.Interface, error) {
			return fake.NewSimpleClientset(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-creds",
					Namespace: "test-ns",
				},
				Data:       map[string][]byte{corev1.DockerConfigJsonKey: expected},
				StringData: nil,
				Type:       corev1.SecretTypeDockerConfigJson,
			}), nil
		}

		credentials := []hephv1.RegistryCredentials{
			{
				Secret: &hephv1.SecretCredentials{
					Name:      "test-creds",
					Namespace: "test-ns",
				},
			},
		}

		zapLogger, err := logger.NewZap(cfg.Logging{})
		if err != nil {
			t.Fatalf("Unexpected error while creating test logger.")
		}

		ctrl.SetLogger(zapr.NewLogger(zapLogger))
		testLog := ctrl.Log.WithName("testLog")

		configPath, err := Persist(context.Background(), testLog, nil, credentials)
		require.NoError(t, err)
		t.Cleanup(func() {
			os.RemoveAll(configPath)
		})

		actual, err := os.ReadFile(filepath.Join(configPath, "config.json"))
		require.NoError(t, err)

		assert.Equal(t, expected, actual)
	})
}
