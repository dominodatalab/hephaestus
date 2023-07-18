package cloudauth

import (
	"context"
	"regexp"
	"testing"

	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/logr"
)

func TestRegistry_RetrieveAuthorization(t *testing.T) {
	expected := &registry.AuthConfig{
		Username: "test-user",
		Password: "test-pass",
	}
	r := &Registry{}
	r.Register(regexp.MustCompile(`^my.cloud`), func(context.Context, logr.Logger, string) (*registry.AuthConfig, error) {
		return expected, nil
	})

	testLog := logr.Discard()

	ctx := context.Background()
	auth, err := r.RetrieveAuthorization(ctx, testLog, "my.cloud/best/cloud")
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if auth != expected {
		t.Errorf("wrong auth: got %v, want %v", auth, expected)
	}

	auth, err = r.RetrieveAuthorization(ctx, testLog, "your.cloud/silly/cloud")
	if err != ErrNoLoader {
		t.Errorf("wrong err: got %v, want %v", err, ErrNoLoader)
	}
	if auth != nil {
		t.Errorf("unexpected auth: got %v", auth)
	}
}
