package acr

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry/containerregistryapi"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/docker/docker/api/types"
	"github.com/go-logr/logr"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

// https://github.com/Azure/acr/blob/main/docs/AAD-OAuth.md

const acrUserForRefreshToken = "00000000-0000-0000-0000-000000000000"

var (
	acrRegex                                           = regexp.MustCompile(`.*\.azurecr\.io|.*\.azurecr\.cn|.*\.azurecr\.de|.*\.azurecr\.us`)
	defaultRefreshTokensClient refreshTokensClientFunc = func(loginURL string) containerregistryapi.RefreshTokensClientAPI {
		obj := containerregistry.NewRefreshTokensClient(loginURL)
		return &obj
	}
	defaultChallengeLoginServer = cloudauth.ChallengeLoginServer
)

type refreshTokensClientFunc func(loginURL string) containerregistryapi.RefreshTokensClientAPI

type refresherWithContextAndOAuthToken interface {
	adal.RefresherWithContext
	adal.OAuthTokenProvider
}

type acrProvider struct {
	tenantID              string
	servicePrincipalToken refresherWithContextAndOAuthToken
}

// Register will instantiate a new authentication provider whenever the AZURE_TENANT_ID or AZURE_CLIENT_ID envvars are
// present, otherwise it will result in a no-op. An error will be returned whenever the envvar settings are invalid.
func Register(ctx context.Context, logger logr.Logger, registry *cloudauth.Registry) error {
	_, tenantIDDefined := os.LookupEnv(auth.TenantID)
	_, clientIDDefined := os.LookupEnv(auth.ClientID)
	if !(tenantIDDefined && clientIDDefined) {
		logger.Info(fmt.Sprintf(
			"ACR authentication provider not registered, %s or %s is absent", auth.TenantID, auth.ClientID,
		))

		return nil
	}

	provider, err := newProvider(ctx, logger)
	if err != nil {
		return fmt.Errorf("failed to create authentication provider: %w", err)
	}

	registry.Register(acrRegex, provider.authenticate)
	logger.Info("ACR authentication provider registered")

	return nil
}

func newProvider(ctx context.Context, logger logr.Logger) (*acrProvider, error) {
	settings, err := auth.GetSettingsFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("cannot get settings from env: %w", err)
	}

	var token *adal.ServicePrincipalToken

	if cc, err := settings.GetClientCredentials(); err == nil {
		if token, err = cc.ServicePrincipalToken(); err != nil {
			return nil, fmt.Errorf("retrieving service principal token failed: %w", err)
		}
	} else {
		err = retry(ctx, logger, 3, func() error {
			token, err = settings.GetMSI().ServicePrincipalToken()
			return err
		})

		if err != nil {
			// IMDS can take some time to set up, restart the process
			return nil, fmt.Errorf("retreiving service principal token from MSI failed: %w", err)
		}
	}

	return &acrProvider{
		tenantID:              settings.Values[auth.TenantID],
		servicePrincipalToken: token,
	}, nil
}

func (a *acrProvider) authenticate(ctx context.Context, logger logr.Logger, server string) (*types.AuthConfig, error) {
	logger = logger.WithName("acr-auth-provider")

	match := acrRegex.FindAllString(server, -1)
	if len(match) != 1 {
		err := fmt.Errorf("ACR URL is invalid: %q should match pattern %v", server, acrRegex)
		logger.Info(err.Error())

		return nil, err
	}

	loginServer := match[0]
	err := retry(ctx, logger, 3, func() error {
		return a.servicePrincipalToken.EnsureFreshWithContext(ctx)
	})
	if err != nil {
		logger.Error(err, "Failed to refresh AAD token.")
		return nil, fmt.Errorf("failed to refresh AAD token: %w", err)
	}

	armAccessToken := a.servicePrincipalToken.OAuthToken()
	loginServerURL := "https://" + loginServer
	directive, err := defaultChallengeLoginServer(ctx, loginServerURL)
	if err != nil {
		logger.Error(err, "ACR cloud authentication failed.")
		return nil, err
	}

	refreshClient := defaultRefreshTokensClient(loginServerURL)
	refreshToken, err := refreshClient.GetFromExchange(
		ctx,
		"access_token",
		directive.Service,
		a.tenantID,
		"",
		armAccessToken,
	)
	if err != nil {
		logger.Error(err, "Token refresh failed.")
		return nil, fmt.Errorf("failed to generate ACR refresh token: %w", err)
	}

	logger.Info("Successfully authenticated with ACR")
	return &types.AuthConfig{
		Username: acrUserForRefreshToken,
		Password: to.String(refreshToken.RefreshToken),
	}, nil
}

func retry(ctx context.Context, logger logr.Logger, attempts int, f func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		err = f()
		if err == nil {
			return nil
		}

		if i == attempts {
			break
		}

		logger.Error(err, "retrying", "attempt", i+1)
		if !autorest.DelayForBackoff(time.Second, i, ctx.Done()) {
			return ctx.Err()
		}
	}

	return err
}
