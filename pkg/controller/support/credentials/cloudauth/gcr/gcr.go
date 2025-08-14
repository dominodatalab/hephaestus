package gcr

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

var defaultChallengeLoginServer = cloudauth.ChallengeLoginServer

var (
	gcrRegex      = regexp.MustCompile(`.*-docker\.pkg\.dev|(?:.*\.)?gcr\.io`)
	defaultClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   2 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
)

type tokenResponse struct {
	Token        string `json:"token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type gcrProvider struct {
	logger      logr.Logger
	tokenSource oauth2.TokenSource
}

func Register(ctx context.Context, logger logr.Logger, registry *cloudauth.Registry) error {
	provider, err := newProvider(ctx, logger)
	if err != nil {
		logger.Info("GCR not registered", "error", err)
		if strings.Contains(err.Error(), "could not find default credentials") {
			return nil
		}
		return err
	}

	registry.Register(gcrRegex, provider.authenticate)
	logger.Info("GCR registered")
	return nil
}

func newProvider(ctx context.Context, logger logr.Logger) (*gcrProvider, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, err
	}

	return &gcrProvider{logger: logger.WithName("gcr-auth-provider"), tokenSource: creds.TokenSource}, nil
}

func (g *gcrProvider) authenticate(
	ctx context.Context,
	logger logr.Logger,
	server string,
) (*registry.AuthConfig, error) {
	match := gcrRegex.FindAllString(server, -1)
	if len(match) != 1 {
		err := fmt.Errorf("invalid GCR URL %s should match %s", server, gcrRegex)
		logger.Info(err.Error())

		return nil, err
	}

	token, err := g.tokenSource.Token()
	if err != nil {
		err = fmt.Errorf("unable to access GCR token from oauth: %w", err)
		logger.Info(err.Error())

		return nil, err
	}

	logger.Info(fmt.Sprintf("Successfully authenticated with GCR %q", server))
	// buildkit only supports username/password
	return &registry.AuthConfig{
		Username:      "oauth2accesstoken",
		Password:      token.AccessToken,
	}, nil
}
