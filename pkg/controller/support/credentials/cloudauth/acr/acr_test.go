package acr

import (
	"context"
	"errors"
	"fmt"
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
	invalidServerErr := errors.New("ACR URL is invalid: \"test-server\" should match pattern .*\\.azurecr\\.io|.*\\.azurecr\\.cn|.*\\.azurecr\\.de|.*\\.azurecr\\.us")
	acrGetExchangeErr := fmt.Errorf("failed to generate ACR refresh token: get from exchange error")
	aadTokenErr := errors.New("failed to refresh AAD token: failed to refresh principal token")
	challengeServerErr := errors.New("failed to refresh AAD token: failed to refresh principal token")

	for _, tt := range []struct {
		name                        string
		serverName                  string
		provider                    *acrProvider
		defaultRefreshTokensClient  func(loginURL string) containerregistryapi.RefreshTokensClientAPI
		defaultChallengeLoginServer func(ctx context.Context, loginServerURL string) (*cloudauth.AuthDirective, error)
		authConfig                  *types.AuthConfig
		expectedLogMessage          string
		expectedError               error
	}{
		{
			"success",
			"foo.azurecr.us",
			createProvider("test-tenantId", fakeServicePrincipalToken{}),
			createDefaultRefreshTokensClient(false),
			createDefaultChallengeLoginServer("test-service", "test-realm", nil),
			&types.AuthConfig{Username: acrUserForRefreshToken},
			"Successfully authenticated with ACR",
			nil,
		},
		{
			"invalid_server",
			"test-server",
			createProvider("test-tenantId", fakeServicePrincipalToken{}),
			createDefaultRefreshTokensClient(false),
			createDefaultChallengeLoginServer("", "", nil),
			nil,
			fmt.Sprintf(`ACR URL is invalid: "test-server" should match pattern %s`, acrRegex),
			invalidServerErr,
		},
		{
			"failed_get_from_exchange",
			"foo.azurecr.us",
			createProvider("test-tenantId", fakeServicePrincipalToken{}),
			createDefaultRefreshTokensClient(true),
			createDefaultChallengeLoginServer("", "", nil),
			nil,
			"Token refresh failed.",
			acrGetExchangeErr,
		},
		{
			"failed_refresh_exchange",
			"foo.azurecr.us",
			createProvider("test-tenantId", fakeServicePrincipalToken{errOut: true}),
			createDefaultRefreshTokensClient(false),
			createDefaultChallengeLoginServer("", "", nil),
			nil,
			"Failed to refresh AAD token.",
			aadTokenErr,
		},
		{
			"failed_default_challenge_login_server",
			"foo.azurecr.us",
			createProvider("test-tenantId", fakeServicePrincipalToken{}),
			createDefaultRefreshTokensClient(false),
			createDefaultChallengeLoginServer("", "", challengeServerErr),
			nil,
			"ACR cloud authentication failed.",
			aadTokenErr,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			observerCore, observedLogs := observer.New(zap.DebugLevel)
			log := zapr.NewLogger(zap.New(observerCore))

			defaultChallengeLoginServer = tt.defaultChallengeLoginServer
			defaultRefreshTokensClient = tt.defaultRefreshTokensClient

			authConfig, err := tt.provider.authenticate(ctx, log, tt.serverName)

			// Compare expected error condition.
			if tt.expectedError == nil {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.expectedError.Error(), err.Error())
			}

			// Compare expected log messages.
			logLen := observedLogs.Len()
			if logLen > 0 {
				recentLogMessage := observedLogs.All()[observedLogs.Len()-1].Message
				assert.Equal(t, tt.expectedLogMessage, recentLogMessage)
			}

			assert.Equal(t, tt.authConfig, authConfig)
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

// helpers
func createProvider(tenantId string, servicePrincipalToken fakeServicePrincipalToken) *acrProvider {
	return &acrProvider{
		tenantID:              tenantId,
		servicePrincipalToken: servicePrincipalToken,
	}
}

func createDefaultChallengeLoginServer(
	serviceName,
	realmName string,
	expectedErr error,
) func(ctx context.Context, loginServerURL string) (*cloudauth.AuthDirective, error) {
	var err error
	if expectedErr != nil {
		err = expectedErr
	}
	return func(ctx context.Context, loginServerURL string) (*cloudauth.AuthDirective, error) {
		return &cloudauth.AuthDirective{
			Service: serviceName,
			Realm:   realmName,
		}, err
	}
}

func createDefaultRefreshTokensClient(errOut bool) func(loginURL string) containerregistryapi.RefreshTokensClientAPI {
	return func(loginURL string) containerregistryapi.RefreshTokensClientAPI {
		return &fakeRefreshTokensClient{
			errOut: errOut,
		}
	}
}
