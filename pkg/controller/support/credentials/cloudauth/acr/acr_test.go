package acr

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry/containerregistryapi"
	"github.com/docker/docker/api/types"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

func TestAuthenticate(t *testing.T) {
	defaultRefreshTokensClient = func(loginURL string) containerregistryapi.RefreshTokensClientAPI {
		return &fakeRefreshTokensClient{}
	}
	defaultChallengeLoginServer = func(ctx context.Context, loginServerURL string) (*cloudauth.AuthDirective, error) {
		return &cloudauth.AuthDirective{
			Service: "test-service",
			Realm:   "test-realm",
		}, nil
	}

	provider := &acrProvider{
		tenantID:              "test-tenant-id",
		servicePrincipalToken: fakeServicePrincipalToken{},
	}

	ctx := context.Background()
	observerCore, observedLogs := observer.New(zap.DebugLevel)
	log := zapr.NewLogger(zap.New(observerCore))

	t.Run("invalid_server", func(t *testing.T) {
		_, err := provider.authenticate(ctx, log, "test-server")

		require.Error(t, err)
		assert.Equal(t, `ACR URL is invalid: "test-server" should match pattern .*\.azurecr\.io|.*\.azurecr\.cn|.*\.azurecr\.de|.*\.azurecr\.us`, observedLogs.All()[0].Message)
	})

	t.Run("success", func(t *testing.T) {
		authConfig, err := provider.authenticate(ctx, log, "foo.azurecr.us")

		require.NoError(t, err)
		assert.Equal(t, &types.AuthConfig{Username: acrUserForRefreshToken}, authConfig)
	})
}

type fakeServicePrincipalToken struct {
	errOut bool
}

func (f fakeServicePrincipalToken) RefreshWithContext(ctx context.Context) error { return nil }

func (f fakeServicePrincipalToken) RefreshExchangeWithContext(ctx context.Context, resource string) error {
	return nil
}

func (f fakeServicePrincipalToken) EnsureFreshWithContext(ctx context.Context) error {
	if f.errOut {
		return errors.New("failed to refresh principal token")
	}

	return nil
}

func (f fakeServicePrincipalToken) OAuthToken() string {
	return "test-oauth-token"
}

type fakeRefreshTokensClient struct{}

func (f fakeRefreshTokensClient) GetFromExchange(ctx context.Context, grantType string, service string, tenant string, refreshToken string, accessToken string) (result containerregistry.RefreshToken, err error) {
	return containerregistry.RefreshToken{}, nil
}
