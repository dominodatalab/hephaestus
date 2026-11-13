package credentials

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginHost(t *testing.T) {
	for server, want := range map[string]string{
		"":                             "registry-1.docker.io",
		"https://index.docker.io/v1/":  "registry-1.docker.io",
		"docker.io":                    "registry-1.docker.io",
		"quay.io":                      "quay.io",
		"https://quay.io/v2/":          "quay.io",
		"reg.example.com:5000":         "reg.example.com:5000",
		"http://reg.example.com:5000/": "reg.example.com:5000",
	} {
		got, err := loginHost(server)
		require.NoError(t, err, server)
		assert.Equal(t, want, got, server)
	}
}

func TestAuthenticate(t *testing.T) {
	t.Run("accepts_basic_credential", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user, pass, ok := r.BasicAuth(); !ok || user != "u" || pass != "p" {
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		host := strings.TrimPrefix(srv.URL, "http://")
		for name, auth := range map[string]registry.AuthConfig{
			"user_pass":  {Username: "u", Password: "p"},
			"auth_field": {Auth: "dTpw"}, // u:p
		} {
			t.Run(name, func(t *testing.T) {
				auth.ServerAddress = host
				assert.NoError(t, authenticate(context.Background(), &auth, []string{host}, "test"))
			})
		}
	})

	t.Run("plain_http_requires_exact_insecure_listing", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		host := strings.TrimPrefix(srv.URL, "http://")
		localhost := strings.Replace(host, "127.0.0.1", "localhost", 1)
		for _, tc := range []struct {
			name     string
			host     string
			insecure []string
			allowed  bool
		}{
			{name: "unlisted", host: host},
			{name: "cidr", host: host, insecure: []string{"127.0.0.0/8"}},
			{name: "missing_port", host: host, insecure: []string{"127.0.0.1"}},
			{name: "different_case", host: localhost, insecure: []string{strings.ToUpper(localhost)}},
			{name: "exact", host: host, insecure: []string{host}, allowed: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				requests.Store(0)
				err := authenticate(context.Background(), &registry.AuthConfig{
					Username: "u", Password: "p", ServerAddress: tc.host,
				}, tc.insecure, "test")
				if tc.allowed {
					assert.NoError(t, err)
					assert.Positive(t, requests.Load())
				} else {
					assert.Error(t, err)
					assert.False(t, unreachable(err), "refusing plain HTTP is not an outage")
					assert.Zero(t, requests.Load(), "an unlisted registry must receive no HTTP requests")
				}
			})
		}
	})

	t.Run("registry_token_is_ignored", func(t *testing.T) {
		var authorization atomic.Value
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"issued-token"}`))
		})
		mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			authorization.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
		})

		host := strings.TrimPrefix(srv.URL, "http://")
		err := authenticate(context.Background(), &registry.AuthConfig{
			Username: "u", Password: "p", RegistryToken: "registry-token", ServerAddress: host,
		}, []string{host}, "test")
		require.NoError(t, err)
		assert.Equal(t, "Bearer issued-token", authorization.Load())
	})

	t.Run("rejects_bad_basic_credential_as_unauthorized", func(t *testing.T) {
		host := respondingHost(t, http.StatusUnauthorized)

		err := authenticate(context.Background(), &registry.AuthConfig{
			Username: "u", Password: "p", ServerAddress: host,
		}, []string{host}, "test")
		require.Error(t, err)
		assert.True(t, errdefs.IsUnauthorized(err), err.Error())
	})

	t.Run("rejects_bearer_401_as_unauthorized", func(t *testing.T) {
		host := bearerHost(t, http.StatusUnauthorized)

		err := authenticate(context.Background(), &registry.AuthConfig{
			Username: "u", Password: "p", ServerAddress: host,
		}, []string{host}, "test")
		require.Error(t, err)
		assert.True(t, errdefs.IsUnauthorized(err), err.Error())
	})

	t.Run("unreachable_registry_is_not_an_answer", func(t *testing.T) {
		host := unreachableHost(t)

		err := authenticate(context.Background(), &registry.AuthConfig{
			Username: "u", Password: "p", ServerAddress: host,
		}, []string{host}, "test")
		require.Error(t, err)
		assert.True(t, unreachable(err), err.Error())
	})

	t.Run("insecure_registry_skips_cert_verification", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		host := strings.TrimPrefix(srv.URL, "https://")
		auth := &registry.AuthConfig{Username: "u", Password: "p", ServerAddress: host}

		assert.NoError(t, authenticate(context.Background(), auth, []string{host}, "test"))
		assert.True(t, certRejected(authenticate(context.Background(), auth, nil, "test")))
	})
}

func TestClassifyAuthError(t *testing.T) {
	t.Run("answer_is_not_masked_by_refused_https", func(t *testing.T) {
		refused := &url.Error{
			Op: "Get", URL: "https://registry.example.com/v2/",
			Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		}
		require.True(t, unreachable(refused))

		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				answer := &transport.Error{StatusCode: status}
				err := classifyAuthError(errors.Join(refused, answer))
				assert.False(t, unreachable(err), err.Error())
				assert.NotErrorIs(t, err, syscall.ECONNREFUSED)
				assert.ErrorIs(t, err, answer)
				assert.Equal(t, status == http.StatusUnauthorized, errdefs.IsUnauthorized(err))
			})
		}
	})
}
