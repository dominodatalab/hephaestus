package credentials

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

func TestPersist(t *testing.T) {
	t.Run("all_secret_auths", func(t *testing.T) {
		config := DockerConfigJSON{
			Auths: AuthConfigs{
				"registry1.com": registry.AuthConfig{
					Username: "happy",
					Password: "gilmore",
				},
				"registry2.com": registry.AuthConfig{
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

		configPath, helpMessage, err := Persist(context.Background(), logr.Discard(), nil, credentials)
		require.NoError(t, err)
		t.Cleanup(func() {
			os.RemoveAll(configPath)
		})

		actual, err := os.ReadFile(filepath.Join(configPath, "config.json"))
		require.NoError(t, err)

		assert.Equal(t, expected, actual)
		assert.Equal(t, len(helpMessage), 1)
		assert.Contains(t, helpMessage[0], "secret \"test-creds\" in namespace \"test-ns\"")
	})
}

// writeDockerConfig writes a config.json holding one credential for the given
// server key and returns its directory.
func writeDockerConfig(t *testing.T, server string) string {
	t.Helper()

	dir := t.TempDir()
	config := DockerConfigJSON{Auths: AuthConfigs{
		server: registry.AuthConfig{Username: "u", Password: "p"},
	}}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0644))

	return dir
}

// unreachableHost returns a host:port that refuses connections, by closing a
// listener and reusing its address.
func unreachableHost(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	return strings.TrimPrefix(srv.URL, "http://")
}

// cancelledContext returns an already cancelled context. Verify aborts before
// contacting a registry it checks, so tests use it to observe whether a
// credential was checked (context.Canceled) or skipped (nil).
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}

func TestVerify(t *testing.T) {
	t.Run("unreachable_registry_is_skipped", func(t *testing.T) {
		// A refused connection is not an unauthorized error, so Verify must
		// skip the registry and let the build continue instead of failing it.
		host := unreachableHost(t)
		dir := writeDockerConfig(t, host)

		// Keep the test quick: one attempt, no long backoff.
		orig := defaultBackoff
		defaultBackoff = wait.Backoff{Duration: time.Millisecond, Steps: 1}
		t.Cleanup(func() { defaultBackoff = orig })

		err := Verify(context.Background(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.NoError(t, err)
	})

	t.Run("cancelled_context_is_fatal", func(t *testing.T) {
		// A cancelled build context must fail the build, not be mistaken for an
		// unreachable registry and skipped.
		host := unreachableHost(t)
		dir := writeDockerConfig(t, host)

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("unused_registry_is_not_checked", func(t *testing.T) {
		// The build pushes to another registry, so this credential must be
		// skipped before any check. With a cancelled context a check would
		// return context.Canceled, so nil proves no check happened.
		dir := writeDockerConfig(t, "unused.example.com")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"used.example.com/org/app:latest"})
		assert.NoError(t, err)
	})

	t.Run("docker_hub_alias_matches", func(t *testing.T) {
		// A legacy Docker Hub server key must match an image ref that
		// normalizes to docker.io, so this credential is checked.
		dir := writeDockerConfig(t, "https://index.docker.io/v1/")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("no_images_checks_all", func(t *testing.T) {
		// Without an image list Verify falls back to checking every
		// credential, so the check runs and reports the cancelled context.
		dir := writeDockerConfig(t, "unused.example.com")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, nil)
		assert.ErrorIs(t, err, context.Canceled)
	})
}
