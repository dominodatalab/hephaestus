//go:build integration

package acr

import (
	"context"
	"os"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

func requireAzure(t *testing.T) string {
	t.Helper()
	if os.Getenv(auth.TenantID) == "" || os.Getenv(auth.ClientID) == "" {
		t.Skip("Skipping, azure not setup")
	}
	acrRegistry := os.Getenv("ACR_REGISTRY")
	if acrRegistry == "" {
		t.Skip("Skipping, ACR_REGISTRY not set")
	}

	return acrRegistry
}

func TestRegisterIntegration(t *testing.T) {
	requireAzure(t)

	err := Register(context.Background(), logr.Discard(), &cloudauth.Registry{})
	require.NoError(t, err)
}

func TestAuthenticateIntegration(t *testing.T) {
	acrRegistry := requireAzure(t)

	ctx := context.Background()
	p, err := newProvider(ctx, logr.Discard(), os.Getenv(auth.TenantID))
	require.NoError(t, err)

	t.Run("invalid url", func(t *testing.T) {
		authConfig, err := p.authenticate(ctx, logr.Discard(), "bogus.azure.io")
		assert.Nil(t, authConfig)
		assert.ErrorIs(t, err, errACRURL)
	})

	t.Run("valid url", func(t *testing.T) {
		authConfig, err := p.authenticate(ctx, logr.Discard(), acrRegistry)
		require.NoError(t, err)
		require.NotNil(t, authConfig)
		require.NotEmpty(t, authConfig.Password)

		// verify the refresh token can be exchanged for a registry access token
		accessClient := containerregistry.NewAccessTokensClient("https://" + acrRegistry)
		r, err := accessClient.Get(ctx, acrRegistry, "repository:hephaestus-integration:pull", authConfig.Password)
		require.NoError(t, err)
		assert.NotEmpty(t, to.String(r.AccessToken))
	})
}
