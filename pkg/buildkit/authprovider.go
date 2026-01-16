package buildkit

import (
	"context"
	"sync"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/logr"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/session/auth/authprovider"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

var authBackoff = wait.Backoff{
	Duration: time.Second,
	Factor:   2,
	Steps:    3,
}

// RefreshingAuthProvider fetches fresh credentials on-demand when BuildKit requests them.
// For cloud registries (ACR/ECR/GCR), it calls the cloud provider to get a fresh token.
// For other registries, it falls back to the static docker config via DockerAuthProvider.
type RefreshingAuthProvider struct {
	auth.UnimplementedAuthServer

	cloudAuth            *cloudauth.Registry
	staticConfigProvider session.Attachable
	logger               logr.Logger
	// mu protects against concurrent Credentials() calls from BuildKit and guards
	// the retry state (authConfig, lastErr) within each call. While cloudAuth.Registry
	// appears thread-safe, this mutex ensures safe access to local variables during retries.
	mu sync.Mutex
}

// NewRefreshingAuthProvider creates a new auth provider that refreshes cloud credentials on-demand.
func NewRefreshingAuthProvider(
	cloudAuth *cloudauth.Registry,
	dockerConfigDir string,
	logger logr.Logger,
) session.Attachable {
	// Load static config and create a DockerAuthProvider for fallback
	staticConfig, _ := config.Load(dockerConfigDir)
	staticConfigProvider := authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		ConfigFile: staticConfig,
		TLSConfigs: nil,
	})

	return &RefreshingAuthProvider{
		cloudAuth:            cloudAuth,
		staticConfigProvider: staticConfigProvider,
		logger:               logger.WithName("refreshing-auth-provider"),
	}
}

// Register implements session.Attachable
func (p *RefreshingAuthProvider) Register(server *grpc.Server) {
	auth.RegisterAuthServer(server, p)
}

// Credentials is called by BuildKit when it needs auth for a registry.
func (p *RefreshingAuthProvider) Credentials(
	ctx context.Context,
	req *auth.CredentialsRequest,
) (*auth.CredentialsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	host := req.GetHost()
	p.logger.V(1).Info("Credentials requested", "host", host)

	// Try cloud auth with retry
	var authConfig *registry.AuthConfig
	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, authBackoff, func(ctx context.Context) (bool, error) {
		var err error
		authConfig, err = p.cloudAuth.RetrieveAuthorization(ctx, p.logger, host)

		if err == cloudauth.ErrNoLoader {
			// Not a cloud registry, stop retrying and fall back to static config
			return true, err
		}
		if err != nil {
			lastErr = err
			p.logger.V(1).Info("Cloud auth failed, retrying", "host", host, "error", err)
			return false, nil // Retry
		}
		return true, nil // Success
	})

	// If cloud auth succeeded, return fresh credentials
	if err == nil && authConfig != nil {
		p.logger.Info("Returning fresh cloud credentials", "host", host, "username", authConfig.Username)
		p.logger.V(1).Info("Cloud credentials retrieved successfully", "host", host, "hasPassword", authConfig.Password != "")
		return &auth.CredentialsResponse{
			Username: authConfig.Username,
			Secret:   authConfig.Password,
		}, nil
	}

	// Log if cloud auth failed (not just "not a cloud registry")
	if err != cloudauth.ErrNoLoader && lastErr != nil {
		p.logger.Error(lastErr, "Cloud auth failed after retries, falling back to static config", "host", host)
	}

	// Delegate to DockerAuthProvider for static config handling
	p.logger.V(1).Info("Falling back to static config provider", "host", host)
	authServer, ok := p.staticConfigProvider.(auth.AuthServer)
	if !ok {
		p.logger.V(1).Info("Static config provider does not implement AuthServer", "host", host)
		return &auth.CredentialsResponse{}, nil
	}
	return authServer.Credentials(ctx, req)
}
