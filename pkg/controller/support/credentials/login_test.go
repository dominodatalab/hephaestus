package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/go-logr/logr"
	typesregistry "github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// basicOKHost accepts only Basic u:p.
func basicOKHost(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok && user == "u" && pass == "p" {
			w.WriteHeader(http.StatusOK)
			return
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

// bearerOKHost issues a token for Basic u:p (GET) or for refresh token "idt"
// (OAuth POST), and accepts only that token on /v2/.
func bearerOKHost(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var ok bool
		switch r.Method {
		case http.MethodPost:
			ok = r.FormValue("grant_type") == "refresh_token" && r.FormValue("refresh_token") == "idt"
		case http.MethodGet:
			user, pass, basic := r.BasicAuth()
			ok = basic && user == "u" && pass == "p"
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok", "access_token": "tok"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok" {
			w.WriteHeader(http.StatusOK)
			return
		}

		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})

	return strings.TrimPrefix(srv.URL, "http://")
}

func TestVerifyAcceptsGoodCredentials(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		host := basicOKHost(t)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.NoError(t, err)
	})

	t.Run("bearer", func(t *testing.T) {
		host := bearerOKHost(t)
		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.NoError(t, err)
	})

	t.Run("no_auth_required", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		host := strings.TrimPrefix(srv.URL, "http://")

		dir := writeDockerConfigFor(t, host)
		shortBackoff(t)

		err := Verify(context.Background(), logr.Discard(), dir, nil, []string{"test"}, []string{host + "/org/app:latest"})
		assert.NoError(t, err)
	})
}

func TestRegistryLogin(t *testing.T) {
	t.Run("identity_token_uses_refresh_grant", func(t *testing.T) {
		host := bearerOKHost(t)
		auth := typesregistry.AuthConfig{ServerAddress: host, IdentityToken: "idt"}

		assert.NoError(t, registryLogin(context.Background(), auth, nil))
	})

	t.Run("bad_identity_token_is_unauthorized", func(t *testing.T) {
		host := bearerOKHost(t)
		auth := typesregistry.AuthConfig{ServerAddress: host, IdentityToken: "wrong"}

		err := registryLogin(context.Background(), auth, nil)
		require.Error(t, err)
		assert.True(t, errdefs.IsUnauthorized(err), "got %v", err)
	})

	t.Run("https_401_is_not_retried_over_http", func(t *testing.T) {
		// Loopback is insecure, so plain HTTP is a candidate. A 401 over HTTPS must
		// end the attempt; the HTTP try against this TLS port would answer 400.
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		host := strings.TrimPrefix(srv.URL, "https://")

		auth := typesregistry.AuthConfig{ServerAddress: host, Username: "u", Password: "p"}
		err := registryLogin(context.Background(), auth, nil)
		require.Error(t, err)
		assert.True(t, errdefs.IsUnauthorized(err), "got %v", err)
		assert.Contains(t, err.Error(), "https://")
	})
}

func TestLoginEndpoints(t *testing.T) {
	ctx := context.Background()

	for _, server := range []string{"https://index.docker.io/v1/", "docker.io", "index.docker.io"} {
		t.Run("docker_hub_"+server, func(t *testing.T) {
			endpoints, err := loginEndpoints(ctx, server, nil)
			require.NoError(t, err)
			require.Len(t, endpoints, 1)
			assert.Equal(t, "https://registry-1.docker.io", endpoints[0].url.String())
			assert.False(t, endpoints[0].tlsConfig.InsecureSkipVerify)
		})
	}

	t.Run("secure_registry_is_https_only", func(t *testing.T) {
		endpoints, err := loginEndpoints(ctx, "https://alice:s3cr3t@203.0.113.10:5000/v1/", nil)
		require.NoError(t, err)
		require.Len(t, endpoints, 1)
		assert.Equal(t, "https://203.0.113.10:5000", endpoints[0].url.String())
		assert.False(t, endpoints[0].tlsConfig.InsecureSkipVerify)
	})

	t.Run("insecure_registry_tries_https_then_http", func(t *testing.T) {
		endpoints, err := loginEndpoints(ctx, "203.0.113.10:5000", []string{"203.0.113.10:5000"})
		require.NoError(t, err)
		require.Len(t, endpoints, 2)
		assert.Equal(t, "https://203.0.113.10:5000", endpoints[0].url.String())
		assert.Equal(t, "http://203.0.113.10:5000", endpoints[1].url.String())
		assert.True(t, endpoints[0].tlsConfig.InsecureSkipVerify)
	})
}

func TestIsInsecureRegistry(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		host     string
		insecure []string
		want     bool
	}{
		{"loopback_ipv4", "127.0.0.1:5000", nil, true},
		{"loopback_ipv6", "[::1]:5000", nil, true},
		{"localhost_resolves_to_loopback", "localhost:5000", nil, true},
		{"listed_host", "reg.example.com:5000", []string{"reg.example.com:5000"}, true},
		{"listed_host_with_scheme", "reg.example.com:5000", []string{"http://reg.example.com:5000"}, true},
		{"listed_host_other_port", "reg.example.com", []string{"reg.example.com:5000"}, false},
		{"listed_cidr", "192.0.2.7:5000", []string{"192.0.2.0/24"}, true},
		// Unlike go-containerregistry, a private address is not insecure by default.
		{"private_ip_not_listed", "10.1.2.3:5000", nil, false},
		{"public_ip_not_listed", "203.0.113.10", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isInsecureRegistry(ctx, tt.host, tt.insecure))
		})
	}
}

func TestConvertToHostname(t *testing.T) {
	tests := map[string]string{
		"https://index.docker.io/v1/": "index.docker.io",
		"http://reg:5000/path":        "reg:5000",
		"reg.example.com":             "reg.example.com",
		"reg.example.com/v1/":         "reg.example.com",
		"oci://reg.example.com":       "oci:",
	}
	for in, want := range tests {
		assert.Equal(t, want, convertToHostname(in), in)
	}
}
