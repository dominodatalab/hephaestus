package buildkit

import (
	"context"
	"sync"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/logr"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

var authBackoff = wait.Backoff{
	Duration: time.Second,
	Factor:   2,
	Steps:    3, // 1s, 2s, 4s
}

// RefreshingAuthProvider fetches fresh credentials on-demand when BuildKit requests them.
// For cloud registries (ACR/ECR/GCR), it calls the cloud provider to get a fresh token.
// For other registries, it falls back to the static docker config.
type RefreshingAuthProvider struct {
	auth.UnimplementedAuthServer

	cloudAuth    *cloudauth.Registry
	staticConfig *configfile.ConfigFile
	logger       logr.Logger
	mu           sync.Mutex
}

// NewRefreshingAuthProvider creates a new auth provider that refreshes cloud credentials on-demand.
func NewRefreshingAuthProvider(
	cloudAuth *cloudauth.Registry,
	dockerConfigDir string,
	logger logr.Logger,
) session.Attachable {
	// Load static config once at initialization (may be nil if not available)
	staticConfig, _ := config.Load(dockerConfigDir)

	return &RefreshingAuthProvider{
		cloudAuth:    cloudAuth,
		staticConfig: staticConfig,
		logger:       logger.WithName("refreshing-auth-provider"),
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

	// If not a cloud registry or cloud auth failed, fall back to static config
	if err != nil && err != cloudauth.ErrNoLoader && lastErr != nil {
		p.logger.Error(lastErr, "Cloud auth failed after retries, falling back to static config", "host", host)
	}

	return p.fallbackToStaticConfig(host)
}

// FetchToken implements the AuthServer interface but is not used for basic username/password auth.
func (p *RefreshingAuthProvider) FetchToken(
	_ context.Context,
	_ *auth.FetchTokenRequest,
) (*auth.FetchTokenResponse, error) {
	return &auth.FetchTokenResponse{}, nil
}

// GetTokenAuthority implements the AuthServer interface but is not used for basic username/password auth.
func (p *RefreshingAuthProvider) GetTokenAuthority(
	_ context.Context,
	_ *auth.GetTokenAuthorityRequest,
) (*auth.GetTokenAuthorityResponse, error) {
	return &auth.GetTokenAuthorityResponse{}, nil
}

// VerifyTokenAuthority implements the AuthServer interface but is not used for basic username/password auth.
func (p *RefreshingAuthProvider) VerifyTokenAuthority(
	_ context.Context,
	_ *auth.VerifyTokenAuthorityRequest,
) (*auth.VerifyTokenAuthorityResponse, error) {
	return &auth.VerifyTokenAuthorityResponse{}, nil
}

//nolint:unparam // error return kept for consistency with auth interface patterns
func (p *RefreshingAuthProvider) fallbackToStaticConfig(host string) (*auth.CredentialsResponse, error) {
	if p.staticConfig == nil {
		p.logger.V(1).Info("No static config available", "host", host)
		return &auth.CredentialsResponse{}, nil
	}

	cfg, err := p.staticConfig.GetAuthConfig(host)
	if err != nil {
		p.logger.Error(err, "Failed to get auth config from static config", "host", host)
		return &auth.CredentialsResponse{}, nil
	}

	if cfg.Username == "" && cfg.Password == "" && cfg.IdentityToken == "" && cfg.RegistryToken == "" {
		p.logger.V(1).Info("No credentials found in static config", "host", host)
		return &auth.CredentialsResponse{}, nil
	}

	p.logger.V(1).Info("Returning credentials from static config", "host", host)
	return &auth.CredentialsResponse{
		Username: cfg.Username,
		Secret:   cfg.Password,
	}, nil
}
