package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// unreachableHost returns a host:port that refuses connections.
func unreachableHost(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	return strings.TrimPrefix(srv.URL, "http://")
}

// respondingHost answers with the given status after a Basic challenge.
func respondingHost(t *testing.T, status int) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

// bearerHost answers a token request with the given status after a Bearer
// challenge. Bearer rejections arrive wrapped in *url.Error, which Basic ones do
// not, so it needs its own coverage. The JSON content type matters: without it
// docker parses the body as a single errcode.Error and a 401 becomes
// unauthorizedErr, a shape real registries never send.
func bearerHost(t *testing.T, status int) string {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"bad creds"}]}`))
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})

	return strings.TrimPrefix(srv.URL, "http://")
}

// rejectThenDieHost 403s authenticated requests, then stops answering. Only the
// last attempt survives in authErr, so this is the shape that hides a bad cred
// behind a later outage. The counter lets the test prove the outage happened: if
// the listener never died, the run proves nothing.
func rejectThenDieHost(t *testing.T, authed *atomic.Int32) string {
	t.Helper()

	var srv *httptest.Server
	var once sync.Once
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		authed.Add(1)
		w.WriteHeader(http.StatusForbidden)
		once.Do(func() { go srv.Close() })
	}))
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

// writeDockerConfigFor writes a config.json with one cred per server.
func writeDockerConfigFor(t *testing.T, servers ...string) string {
	t.Helper()

	auths := AuthConfigs{}
	for _, server := range servers {
		auths[server] = registry.AuthConfig{Username: "u", Password: "p"}
	}

	dir := t.TempDir()
	data, err := json.Marshal(DockerConfigJSON{Auths: auths})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0644))

	return dir
}

// cancelledContext returns a cancelled context. Verify aborts before contacting a
// registry it checks, so context.Canceled means checked and nil means skipped.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}

// shortBackoff makes Verify try once with no wait.
func shortBackoff(t *testing.T) {
	t.Helper()

	orig := defaultBackoff
	defaultBackoff = wait.Backoff{Duration: time.Millisecond, Steps: 1}
	t.Cleanup(func() { defaultBackoff = orig })
}

func TestVerify(t *testing.T) {
	t.Run("unreachable_registry_is_skipped", func(t *testing.T) {
		// Refused connection, so skip and let the build run.
		host := unreachableHost(t)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.NoError(t, err)
	})

	t.Run("unauthorized_registry_is_fatal", func(t *testing.T) {
		// It answered, so the cred is wrong. Fail before leasing a worker.
		host := respondingHost(t, http.StatusUnauthorized)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, []string{host}, []string{"test"}, []string{host + "/org/app:latest"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client credentials are invalid")
	})

	t.Run("forbidden_registry_is_fatal", func(t *testing.T) {
		// Not an outage either. Only no-response gets skipped.
		host := respondingHost(t, http.StatusForbidden)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, []string{host}, []string{"test"}, []string{host + "/org/app:latest"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client credentials are invalid")
	})

	t.Run("bearer_unauthorized_registry_is_fatal", func(t *testing.T) {
		// The bearer 401 arrives inside a *url.Error, which satisfies net.Error.
		// Reading that as an outage is the bug this guards.
		host := bearerHost(t, http.StatusUnauthorized)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, []string{host}, []string{"test"}, []string{host + "/org/app:latest"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client credentials are invalid")
	})

	t.Run("bearer_forbidden_registry_is_fatal", func(t *testing.T) {
		// Same for 403. It still answered.
		host := bearerHost(t, http.StatusForbidden)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, []string{host}, []string{"test"}, []string{host + "/org/app:latest"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client credentials are invalid")
	})

	t.Run("skipped_registry_does_not_hide_a_bad_credential", func(t *testing.T) {
		// Skipping one registry must not bury another's cred failure.
		down := unreachableHost(t)
		bad := respondingHost(t, http.StatusUnauthorized)
		dir := writeDockerConfigFor(t, down, bad)
		shortBackoff(t)

		//nolint:lll
		err := Verify(context.Background(), logr.Discard(), dir, []string{bad}, []string{"test"}, []string{down + "/org/app:latest", bad + "/org/app:latest"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), bad)
		assert.NotContains(t, err.Error(), down)
	})

	t.Run("rejection_is_not_erased_by_a_later_outage", func(t *testing.T) {
		// The registry rejected the cred, then went down. The build must still
		// fail: the outage arrived second and does not clear the rejection.
		var authed atomic.Int32
		host := rejectThenDieHost(t, &authed)
		dir := writeDockerConfigFor(t, host)

		orig := defaultBackoff
		defaultBackoff = wait.Backoff{Duration: 300 * time.Millisecond, Steps: 3}
		t.Cleanup(func() { defaultBackoff = orig })

		err := Verify(context.Background(), logr.Discard(), dir, []string{host}, []string{"test"}, []string{host + "/org/app:latest"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "client credentials are invalid")
		// Without this the run could be three 403s, which passes either way and
		// would quietly stop being a regression test.
		require.Equal(t, int32(1), authed.Load(), "registry answered more than once, so no outage followed the rejection")
	})

	t.Run("cancelled_context_is_fatal", func(t *testing.T) {
		// A cancelled context is not an unreachable registry.
		host := unreachableHost(t)
		dir := writeDockerConfigFor(t, host)

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("non_target_registry_is_not_checked", func(t *testing.T) {
		// Not a target, so never checked. nil proves it, since a check would
		// return context.Canceled.
		dir := writeDockerConfigFor(t, "unused.example.com")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"used.example.com/org/app:latest"})
		assert.NoError(t, err)
	})

	t.Run("docker_hub_alias_matches", func(t *testing.T) {
		// Legacy hub key must match a ref that normalizes to docker.io.
		dir := writeDockerConfigFor(t, "https://index.docker.io/v1/")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("no_refs_checks_all", func(t *testing.T) {
		dir := writeDockerConfigFor(t, "unused.example.com")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, nil)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("host_case_is_ignored", func(t *testing.T) {
		// Case-only difference is the same registry.
		dir := writeDockerConfigFor(t, "MyReg.Example.com")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"myreg.example.com/org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("server_key_userinfo_is_stripped", func(t *testing.T) {
		// Creds embedded in a key must not stop it matching.
		dir := writeDockerConfigFor(t, "https://alice:s3cr3t@myreg.example.com/v1/")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"myreg.example.com/org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("cache_import_registry_is_checked", func(t *testing.T) {
		// Cache imports are pulls, so a cache-only registry is still checked.
		dir := writeDockerConfigFor(t, "cache.example.com")

		//nolint:lll
		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"push.example.com/org/app:latest", "cache.example.com/org/app-cache:latest"})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("unparseable_ref_checks_all", func(t *testing.T) {
		dir := writeDockerConfigFor(t, "unused.example.com")
		badRef := "aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999"

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{badRef})
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("port_is_part_of_the_host", func(t *testing.T) {
		// A non-default port is a distinct host: same port matches, no port does not.
		dir := writeDockerConfigFor(t, "myreg.example.com:5000")

		err := Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"myreg.example.com:5000/org/app:latest"})
		assert.ErrorIs(t, err, context.Canceled)

		err = Verify(cancelledContext(), logr.Discard(), dir, nil, []string{"test"}, []string{"myreg.example.com/org/app:latest"})
		assert.NoError(t, err)
	})
}
