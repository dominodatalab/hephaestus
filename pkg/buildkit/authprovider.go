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

	// Check static config first (credentials from secrets persisted at build start)
	authServer, ok := p.staticConfigProvider.(auth.AuthServer)
	if ok {
		resp, err := authServer.Credentials(ctx, req)
		if err == nil && (resp.GetUsername() != "" || resp.GetSecret() != "") {
			p.logger.V(1).Info("Using static credentials from docker config", "host", host)
			return resp, nil
		}
	}

	// No static credentials for this host, try cloud auth with retry
	p.logger.V(1).Info("No static credentials found, trying cloud auth", "host", host)

	var authConfig *registry.AuthConfig
	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, authBackoff, func(ctx context.Context) (bool, error) {
		var err error
		authConfig, err = p.cloudAuth.RetrieveAuthorization(ctx, p.logger, host)

		if err == cloudauth.ErrNoLoader {
			// Not a cloud registry, stop retrying
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
		return &auth.CredentialsResponse{
			Username: authConfig.Username,
			Secret:   authConfig.Password,
		}, nil
	}

	// Log if cloud auth failed (not just "not a cloud registry")
	if err != cloudauth.ErrNoLoader && lastErr != nil {
		p.logger.Error(lastErr, "Cloud auth failed after retries", "host", host)
	}

	// No credentials found
	p.logger.V(1).Info("No credentials found for host", "host", host)
	return &auth.CredentialsResponse{}, nil
}
