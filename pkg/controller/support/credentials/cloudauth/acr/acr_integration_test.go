//go:build integration
// +build integration

package acr

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/go-logr/logr"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

func TestRegisterIntegration(t *testing.T) {
	if os.Getenv(auth.ClientSecret) == "" {
		t.Skip("Skipping, azure not setup")
	}

	registry := &cloudauth.Registry{}

	err := Register(logr.Discard(), registry)
	if err != nil {
		t.Error(err)
	}
}

func TestRegisterNoSecretIntegration(t *testing.T) {
	secret := os.Getenv(auth.ClientSecret)
	os.Unsetenv(auth.ClientSecret)
	t.Cleanup(func() {
		os.Setenv(auth.ClientSecret, secret)
	})

	registry := &cloudauth.Registry{}
	err := Register(logr.Discard(), registry)
	if err == nil {
		t.Error("expecting an error")
	} else if !strings.Contains(err.Error(), "MSI not available") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestAuthenticateIntegration(t *testing.T) {
	if os.Getenv(auth.ClientSecret) == "" {
		t.Skip("Skipping, azure not setup")
	}

	acrRegistry := os.Getenv("ACR_REGISTRY")
	if len(acrRegistry) == 0 {
		t.Fatal("must set ACR_REGISTRY environment variable")
	}

	p, err := newProvider(logr.Discard())
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	t.Run("invalid url", func(t *testing.T) {
		authConfig, err := p.authenticate(ctx, "bogus.azure.io")
		if authConfig != nil {
			t.Errorf("auth not nil")
		}
		if !strings.HasPrefix(err.Error(), "invalid acr url") {
			t.Fatalf("wrong error: %s", err)
		}
	})

	t.Run("valid url", func(t *testing.T) {
		authConfig, err := p.authenticate(ctx, acrRegistry)
		if err != nil {
			t.Fatalf("%#v", err)
		}
		if len(authConfig.Username) == 0 || len(authConfig.Password) == 0 {
			t.Fatalf("incorrect auth config: %v", authConfig)
		}

		// verify we can obtain an access token from the refresh token
		accessClient := containerregistry.NewAccessTokensClient("https://" + acrRegistry)
		r, err := accessClient.Get(ctx, acrRegistry, "repository:repo:pull,push", auth.Password)
		if err != nil {
			t.Fatal(err)
		}
		if a := to.String(r.AccessToken); len(a) == 0 {
			t.Fatalf("invalid access token: %v", r)
		}
	})
}
