//go:build integration
// +build integration

package gcr

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/go-logr/logr"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

var envCredentials = "GOOGLE_APPLICATION_CREDENTIALS"

func TestRegister(t *testing.T) {
	if os.Getenv(envCredentials) == "" {
		t.Skip("Skipping, gcp not setup")
	}

	registry := &cloudauth.Registry{}

	err := Register(context.TODO(), logr.Discard(), registry)
	if err != nil {
		t.Error(err)
	}
}

func TestRegisterNoCredentials(t *testing.T) {
	secret := os.Getenv(envCredentials)
	os.Unsetenv(envCredentials)
	t.Cleanup(func() {
		os.Setenv(envCredentials, secret)
	})

	registry := &cloudauth.Registry{}
	err := Register(context.TODO(), logr.Discard(), registry)
	if err != nil {
		t.Error(err)
	}
}

func TestAuthenticate(t *testing.T) {
	if os.Getenv(envCredentials) == "" {
		t.Skip("Skipping, gcp not setup")
	}

	p, err := newProvider(context.TODO(), logr.Discard())
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	t.Run("invalid url", func(t *testing.T) {
		auth, err := p.authenticate(ctx, "bogus.g.io")
		if err == nil {
			t.Error("unexpected error")
		}
		if auth != nil {
			t.Errorf("auth not nil")
		}
	})

	t.Run("valid url", func(t *testing.T) {
		for _, tt := range []struct {
			name string
		}{
			{"gcr.io"},
			{"us-west1-docker.pkg.dev"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				auth, err := p.authenticate(ctx, tt.name)
				if err != nil {
					t.Fatalf("%#v", err)
				}
				if auth.Username == "" || auth.Password == "" || auth.RegistryToken == "" {
					t.Fatalf("incorrect auth config: %v", auth)
				}

				// verify the registry token
				req, err := http.NewRequestWithContext(ctx, "GET", "https://"+tt.name+"/v2/", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", auth.RegistryToken))
				resp, err := defaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != 200 {
					t.Fatalf("non 200 status code: %d", resp.StatusCode)
				}
			})
		}
	})
}
