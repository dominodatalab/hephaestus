package acr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/cloudauthtest"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry/containerregistryapi"
	"github.com/docker/docker/api/types"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAuthenticate(t *testing.T) {
	for _, tt := range []struct {
		name                       string
		serverName                 string
		provider                   *acrProvider
		defaultRefreshTokensClient refreshTokensClientFunc
		fakeChallengeLoginServer   cloudauth.LoginChallenger
		authConfig                 *types.AuthConfig
		expectedLogMessage         string
		expectedError              error
	}{
		{
			"success server - foo.azurecr.us",
			"foo.azurecr.us",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&types.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.us\"",
			nil,
		},
		{
			"success server - foo.azurecr.cn",
			"foo.azurecr.cn",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&types.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.cn\"",
			nil,
		},
		{
			"success server - foo.azurecr.de",
			"foo.azurecr.de",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&types.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.de\"",
			nil,
		},
		{
			"success server - foo.azurecr.io",
			"foo.azurecr.io",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&types.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.io\"",
			nil,
		},
		{
			"invalid_server",
			"test-server",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("", "", nil),
			nil,
			fmt.Sprintf(`ACR URL is invalid: "test-server" should match pattern %s`, acrRegex),
			errors.New("ACR URL is invalid: \"test-server\" should match pattern .*\\.azurecr\\.io|.*\\.azurecr\\.cn|.*\\.azurecr\\.de|.*\\.azurecr\\.us"),
		},
		{
			"failed_get_from_exchange",
			"foo.azurecr.cn",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(true),
			cloudauthtest.FakeChallengeLoginServer("", "", nil),
			nil,
			"failed to generate ACR refresh token: get from exchange error",
			fmt.Errorf("failed to generate ACR refresh token: get from exchange error"),
		},
		{
			"failed_refresh_exchange",
			"foo.azurecr.de",
			createProvider("test-tenantId", true),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("", "", nil),
			nil,
			"AAD token refresh failure: failed to refresh principal token",
			errors.New("AAD token refresh failure: failed to refresh principal token"),
		},
		{
			"failed_default_challenge_login_server",
			"foo.azurecr.io",
			createProvider("test-tenantId", false),
			createDefaultRefreshTokensClient(false),
			cloudauthtest.FakeChallengeLoginServer("", "",
				errors.New("failed to refresh AAD token: failed to refresh principal token")),
			nil,
			"ACR registry \"https://foo.azurecr.io\" is unusable: failed to refresh AAD token: failed to refresh principal token",
			errors.New("ACR registry \"https://foo.azurecr.io\" is unusable: failed to refresh AAD token: failed to refresh principal token"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			observerCore, observedLogs := observer.New(zap.DebugLevel)
			log := zapr.NewLogger(zap.New(observerCore))

			defaultChallengeLoginServer = tt.fakeChallengeLoginServer
			defaultRefreshTokensClient = tt.defaultRefreshTokensClient

			authConfig, err := tt.provider.authenticate(ctx, log, tt.serverName)
			assert.Equal(t, tt.authConfig, authConfig)

			// Compare expected error condition.
			if tt.expectedError == nil {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.expectedError.Error(), err.Error())
			}

			// Compare expected log messages.
			logLen := observedLogs.Len()
			require.GreaterOrEqual(t, logLen, 1)
			recentLogMessage := observedLogs.All()[observedLogs.Len()-1].Message
			assert.Equal(t, tt.expectedLogMessage, recentLogMessage)
		})
	}
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

type fakeRefreshTokensClient struct {
	errOut bool
}

func (f fakeRefreshTokensClient) GetFromExchange(_ context.Context, _, _, _, _, _ string) (result containerregistry.RefreshToken, err error) {
	if f.errOut {
		err = errors.New("get from exchange error")
	}
	return
}

// Helper functions
func createProvider(tenantId string, shouldErr bool) *acrProvider {
	return &acrProvider{
		tenantID: tenantId,
		servicePrincipalToken: fakeServicePrincipalToken{
			errOut: shouldErr,
		},
	}
}

func createDefaultRefreshTokensClient(errOut bool) refreshTokensClientFunc {
	return func(loginURL string) containerregistryapi.RefreshTokensClientAPI {
		return &fakeRefreshTokensClient{
			errOut: errOut,
		}
	}
}
