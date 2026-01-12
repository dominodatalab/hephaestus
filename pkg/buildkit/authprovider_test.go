package buildkit

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/logr"
	"github.com/moby/buildkit/session/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

// asRefreshingAuthProvider is a helper to cast session.Attachable to *RefreshingAuthProvider for testing
func asRefreshingAuthProvider(t *testing.T, provider interface{}) *RefreshingAuthProvider {
	rap, ok := provider.(*RefreshingAuthProvider)
	require.True(t, ok, "Expected RefreshingAuthProvider type")
	return rap
}

// mockCloudAuthRegistry is a test double for cloudauth.Registry
type mockCloudAuthRegistry struct {
	authConfig *registry.AuthConfig
	err        error
	callCount  int
}

func (m *mockCloudAuthRegistry) RetrieveAuthorization(ctx context.Context, logger logr.Logger, server string) (*registry.AuthConfig, error) {
	m.callCount++
	return m.authConfig, m.err
}

func TestRefreshingAuthProvider_Credentials_CloudRegistry(t *testing.T) {
	// Setup mock that returns fresh credentials
	mock := &mockCloudAuthRegistry{
		authConfig: &registry.AuthConfig{
			Username: "00000000-0000-0000-0000-000000000000",
			Password: "fresh-acr-token",
		},
	}

	// Create real cloudauth.Registry and register the mock loader
	cloudRegistry := &cloudauth.Registry{}
	cloudRegistry.Register(regexp.MustCompile(`.*\.azurecr\.io`), func(ctx context.Context, logger logr.Logger, server string) (*registry.AuthConfig, error) {
		return mock.RetrieveAuthorization(ctx, logger, server)
	})

	provider := asRefreshingAuthProvider(t, NewRefreshingAuthProvider(cloudRegistry, "", logr.Discard()))

	// Test
	resp, err := provider.Credentials(context.Background(), &auth.CredentialsRequest{
		Host: "myregistry.azurecr.io",
	})

	// Verify
	require.NoError(t, err)
	assert.Equal(t, "00000000-0000-0000-0000-000000000000", resp.Username)
	assert.Equal(t, "fresh-acr-token", resp.Secret)
	assert.Equal(t, 1, mock.callCount)
}

func TestRefreshingAuthProvider_Credentials_Fallback(t *testing.T) {
	// Setup empty cloud registry (no loaders registered)
	cloudRegistry := &cloudauth.Registry{}

	// Create provider with static config directly in-memory
	staticConfig := configfile.New("config.json")
	staticConfig.AuthConfigs = map[string]types.AuthConfig{
		"docker.io": {
			Username: "static-user",
			Password: "static-pass",
		},
	}

	provider := &RefreshingAuthProvider{
		cloudAuth:    cloudRegistry,
		staticConfig: staticConfig,
		logger:       logr.Discard(),
	}

	// Test
	resp, err := provider.Credentials(context.Background(), &auth.CredentialsRequest{
		Host: "docker.io",
	})

	// Verify fallback to static config
	require.NoError(t, err)
	assert.Equal(t, "static-user", resp.Username)
	assert.Equal(t, "static-pass", resp.Secret)
}

func TestRefreshingAuthProvider_Credentials_MultipleCalls(t *testing.T) {
	callCount := 0
	// Setup cloud registry that tracks calls
	cloudRegistry := &cloudauth.Registry{}
	cloudRegistry.Register(regexp.MustCompile(`.*\.azurecr\.io`), func(ctx context.Context, logger logr.Logger, server string) (*registry.AuthConfig, error) {
		callCount++
		return &registry.AuthConfig{
			Username: "user",
			Password: "fresh-token",
		}, nil
	})

	provider := asRefreshingAuthProvider(t, NewRefreshingAuthProvider(cloudRegistry, "", logr.Discard()))

	// Simulate BuildKit asking for credentials multiple times (pull, push, cache)
	for i := 0; i < 3; i++ {
		_, err := provider.Credentials(context.Background(), &auth.CredentialsRequest{
			Host: "myregistry.azurecr.io",
		})
		require.NoError(t, err)
	}

	// Verify: should call cloud auth each time (fresh token each time)
	assert.Equal(t, 3, callCount, "Expected cloud auth to be called for each credential request")
}

func TestRefreshingAuthProvider_Credentials_RetryOnFailure(t *testing.T) {
	callCount := 0
	// Setup cloud registry that fails twice then succeeds
	cloudRegistry := &cloudauth.Registry{}
	cloudRegistry.Register(regexp.MustCompile(`.*\.azurecr\.io`), func(ctx context.Context, logger logr.Logger, server string) (*registry.AuthConfig, error) {
		callCount++
		if callCount < 3 {
			return nil, errors.New("transient error")
		}
		return &registry.AuthConfig{
			Username: "user",
			Password: "token-after-retry",
		}, nil
	})

	provider := asRefreshingAuthProvider(t, NewRefreshingAuthProvider(cloudRegistry, "", logr.Discard()))

	// Test
	resp, err := provider.Credentials(context.Background(), &auth.CredentialsRequest{
		Host: "myregistry.azurecr.io",
	})

	// Verify retry succeeded
	require.NoError(t, err)
	assert.Equal(t, "token-after-retry", resp.Secret)
	assert.GreaterOrEqual(t, callCount, 3, "Expected at least 3 attempts")
}

func TestRefreshingAuthProvider_Register(t *testing.T) {
	provider := NewRefreshingAuthProvider(&cloudauth.Registry{}, "", logr.Discard())

	// Verify it returns a non-nil provider (implements session.Attachable)
	require.NotNil(t, provider)

	// Type assertion to verify it's the right type
	_, ok := provider.(*RefreshingAuthProvider)
	assert.True(t, ok, "Expected RefreshingAuthProvider type")
}
