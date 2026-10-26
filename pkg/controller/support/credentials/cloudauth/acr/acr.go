package acr

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	cra "github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry/containerregistryapi"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/docker/docker/api/types/registry"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/go-logr/logr"
)

// https://github.com/Azure/acr/blob/main/docs/AAD-OAuth.md

const acrUserForRefreshToken = "00000000-0000-0000-0000-000000000000"

// acrTokenScopeEnvVar overrides the scope derived from the registry's cloud.
const acrTokenScopeEnvVar = "HEPHAESTUS_ACR_TOKEN_SCOPE"

const defaultARMScope = "https://management.core.windows.net/.default"

// armScopes maps ACR login server suffixes to the ARM audience of the matching Azure cloud.
var armScopes = map[string]string{
	".azurecr.io": defaultARMScope,
	".azurecr.us": "https://management.core.usgovcloudapi.net/.default",
	".azurecr.cn": "https://management.core.chinacloudapi.cn/.default",
}

var (
	acrRegex  = regexp.MustCompile(`.*\.azurecr\.io|.*\.azurecr\.cn|.*\.azurecr\.de|.*\.azurecr\.us`)
	errACRURL = fmt.Errorf("ACR URL is invalid, should match pattern %v", acrRegex)

	defaultRefreshTokensClient refreshTokensClientFunc = func(loginURL string) cra.RefreshTokensClientAPI {
		obj := containerregistry.NewRefreshTokensClient(loginURL)
		return &obj
	}
)
var defaultChallengeLoginServer = cloudauth.ChallengeLoginServer

type refreshTokensClientFunc func(loginURL string) cra.RefreshTokensClientAPI

type acrProvider struct {
	tenantID        string
	tokenCredential azcore.TokenCredential
	scopeOverride   string
}

// tokenScope returns the override if set, otherwise the ARM audience for the registry's cloud.
func (a *acrProvider) tokenScope(loginServer string) string {
	if a.scopeOverride != "" {
		return a.scopeOverride
	}
	for suffix, scope := range armScopes {
		if strings.HasSuffix(loginServer, suffix) {
			return scope
		}
	}

	return defaultARMScope
}

// Register will instantiate a new authentication provider whenever the AZURE_TENANT_ID or AZURE_CLIENT_ID envvars are
// present, otherwise it will result in a no-op. An error will be returned whenever the envvar settings are invalid.
func Register(ctx context.Context, logger logr.Logger, registry *cloudauth.Registry) error {
	tenantID, tenantIDDefined := os.LookupEnv(auth.TenantID)
	_, clientIDDefined := os.LookupEnv(auth.ClientID)
	if !tenantIDDefined || !clientIDDefined {
		logger.Info(fmt.Sprintf(
			"ACR authentication provider not registered, %s or %s is absent", auth.TenantID, auth.ClientID,
		))

		return nil
	}

	provider, err := newProvider(ctx, logger, tenantID)
	if err != nil {
		return fmt.Errorf("failed to create authentication provider: %w", err)
	}

	registry.Register(acrRegex, provider.authenticate)
	logger.Info("ACR authentication provider registered")

	return nil
}

func newProvider(_ context.Context, _ logr.Logger, tenantID string) (*acrProvider, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("cannot get settings from env: %w", err)
	}

	return &acrProvider{
		tenantID:        tenantID,
		tokenCredential: cred,
		scopeOverride:   os.Getenv(acrTokenScopeEnvVar),
	}, nil
}

func (a *acrProvider) authenticate(
	ctx context.Context,
	logger logr.Logger,
	server string,
) (*registry.AuthConfig, error) {
	logger = logger.WithName("acr-auth-provider")

	match := acrRegex.FindAllString(server, -1)
	if len(match) != 1 {
		err := errACRURL
		logger.Error(err, "Invalid ACR URL", "server", server)

		return nil, err
	}
	loginServer := match[0]

	scope := a.tokenScope(loginServer)
	armAccessToken, err := a.tokenCredential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{scope},
	})
	if err != nil {
		logger.Error(err, "Failed to GetToken.", "loginServer", loginServer, "scope", scope)
		return nil, err
	}
	loginServerURL := "https://" + loginServer
	directive, err := defaultChallengeLoginServer(ctx, loginServerURL)
	if err != nil {
		err = fmt.Errorf("ACR registry login failed: %w", err)
		logger.Error(err, "Login challenge failed.", "loginServer", loginServerURL)

		return nil, err
	}

	refreshClient := defaultRefreshTokensClient(loginServerURL)
	refreshToken, err := refreshClient.GetFromExchange(
		ctx,
		"access_token",
		directive.Service,
		a.tenantID,
		"",
		armAccessToken.Token,
	)
	if err != nil {
		err = fmt.Errorf("failed to generate ACR refresh token: %w", err)
		logger.Info(err.Error())

		return nil, err
	}

	logger.Info(fmt.Sprintf("Successfully authenticated with ACR %q", server))
	return &registry.AuthConfig{
		Username: acrUserForRefreshToken,
		Password: to.String(refreshToken.RefreshToken),
	}, nil
}
