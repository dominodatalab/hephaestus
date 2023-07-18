package acr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry/containerregistryapi"
	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/cloudauthtest"
)

func TestAuthenticate(t *testing.T) {
	errGetToken := errors.New("GetToken failed")
	errExchange := errors.New("get from exchange error")
	for _, tt := range []struct {
		name                     string
		serverName               string
		provider                 *acrProvider
		refreshTokensClient      refreshTokensClientFunc
		fakeChallengeLoginServer cloudauth.LoginChallenger
		authConfig               *registry.AuthConfig
		expectedLogMessage       string
		expectedError            error
	}{
		{
			"success_foo.azurecr.us",
			"foo.azurecr.us",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&registry.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.us\"",
			nil,
		},
		{
			"success_foo.azurecr.cn",
			"foo.azurecr.cn",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&registry.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.cn\"",
			nil,
		},
		{
			"success_foo.azurecr.de",
			"foo.azurecr.de",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&registry.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.de\"",
			nil,
		},
		{
			"success_foo.azurecr.io",
			"foo.azurecr.io",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("test-service", "test-realm", nil),
			&registry.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR \"foo.azurecr.io\"",
			nil,
		},
		{
			"invalid_server",
			"test-server",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("", "", nil),
			nil,
			fmt.Sprintf("Invalid ACR URL"),
			errACRURL,
		},
		{
			"failed_get_from_exchange",
			"foo.azurecr.cn",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(errExchange),
			cloudauthtest.FakeChallengeLoginServer("", "", nil),
			nil,
			"failed to generate ACR refresh token: get from exchange error",
			errExchange,
		},
		{
			"failed_get_token",
			"foo.azurecr.de",
			createProvider("test-tenantId", errGetToken),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("", "", nil),
			nil,
			"Failed to GetToken.",
			errGetToken,
		},
		{
			"failed_default_challenge_login_server",
			"foo.azurecr.io",
			createProvider("test-tenantId", nil),
			createRefreshTokensClient(nil),
			cloudauthtest.FakeChallengeLoginServer("", "", errGetToken),
			nil,
			"Login challenge failed.",
			errGetToken,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			observerCore, observedLogs := observer.New(zap.DebugLevel)
			log := zapr.NewLogger(zap.New(observerCore))

			defaultChallengeLoginServer = tt.fakeChallengeLoginServer
			defaultRefreshTokensClient = tt.refreshTokensClient

			authConfig, err := tt.provider.authenticate(ctx, log, tt.serverName)
			assert.Equal(t, tt.authConfig, authConfig)

			// Compare expected error condition.
			if tt.expectedError == nil {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.expectedError)
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
	err error
}

func (f fakeServicePrincipalToken) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{
		Token: "test-oauth-token",
	}, f.err
}

type fakeRefreshTokensClient struct {
	err error
}

func (f fakeRefreshTokensClient) GetFromExchange(_ context.Context, _, _, _, _, _ string) (result containerregistry.RefreshToken, err error) {
	return result, f.err
}

// Helper functions
func createProvider(tenantId string, err error) *acrProvider {
	return &acrProvider{
		tenantID: tenantId,
		tokenCredential: fakeServicePrincipalToken{
			err: err,
		},
	}
}

func createRefreshTokensClient(err error) refreshTokensClientFunc {
	return func(loginURL string) containerregistryapi.RefreshTokensClientAPI {
		return &fakeRefreshTokensClient{
			err: err,
		}
	}
}
