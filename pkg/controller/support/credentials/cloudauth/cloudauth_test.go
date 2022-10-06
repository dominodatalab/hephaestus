package cloudauth

import (
	"context"
	cfg "github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/logger"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"regexp"
	ctrl "sigs.k8s.io/controller-runtime"
	"testing"

	"github.com/docker/docker/api/types"
)

func TestRegistry_RetrieveAuthorization(t *testing.T) {
	expected := &types.AuthConfig{
		Username: "test-user",
		Password: "test-pass",
	}
	registry := &Registry{}
	registry.Register(regexp.MustCompile(`^my.cloud`), func(context.Context, logr.Logger, string) (*types.AuthConfig, error) {
		return expected, nil
	})
	zapLogger, err := logger.NewZap(cfg.Logging{})
	if err != nil {
		t.Fatalf("Unexpected error while creating test logger.")
	}

	ctrl.SetLogger(zapr.NewLogger(zapLogger))
	testLog := ctrl.Log.WithName("testLog")

	ctx := context.Background()
	auth, err := registry.RetrieveAuthorization(ctx, testLog, "my.cloud/best/cloud")
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if auth != expected {
		t.Errorf("wrong auth: got %v, want %v", auth, expected)
	}

	auth, err = registry.RetrieveAuthorization(ctx, testLog, "your.cloud/silly/cloud")
	if err != ErrNoLoader {
		t.Errorf("wrong err: got %v, want %v", err, ErrNoLoader)
	}
	if auth != nil {
		t.Errorf("unexpected auth: got %v", auth)
	}
}
