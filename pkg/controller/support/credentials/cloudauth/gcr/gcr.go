package gcr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

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

func (g *gcrProvider) authenticate(ctx context.Context, logger logr.Logger, server string) (*types.AuthConfig, error) {
	logger.WithName("gcr-auth-provider")

	match := gcrRegex.FindAllString(server, -1)
	if len(match) != 1 {
		logger.V(2).Info(fmt.Sprintf("Invalid gcr url %s should match %s", server, gcrRegex))
		return nil, fmt.Errorf("invalid gcr url: %q should match %v", server, gcrRegex)
	}

	token, err := g.tokenSource.Token()
	if err != nil {
		logger.Error(err, "Unable to access gcr token.")
		return nil, err
	}

	loginServerURL := "https://" + match[0]
	directive, err := cloudauth.ChallengeLoginServer(ctx, loginServerURL)
	if err != nil {
		logger.Error(err, "Failed gcr cloud authentication.")
		return nil, err
	}

	// obtain the registry token
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directive.Realm, nil)
	if err != nil {
		logger.Error(err, "Unable to access registry token.")
		return nil, err
	}

	v := url.Values{}
	v.Set("service", directive.Service)
	v.Set("client_id", "forge")
	req.URL.RawQuery = v.Encode()
	req.URL.User = url.UserPassword("oauth2accesstoken", token.AccessToken)
	resp, err := defaultClient.Do(req)
	if err != nil {
		logger.Error(err, fmt.Sprintf("Unable to make a request to: %s", req.URL))
		return nil, err
	}

	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(err, "Unable to read response body.")
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		logger.Info(fmt.Sprintf("Failed to obtain token. Received status code: %d", resp.StatusCode))
		return nil, fmt.Errorf("failed to obtain token:\n %s", content)
	}

	var response tokenResponse
	if err := json.Unmarshal(content, &response); err != nil {
		logger.Error(err, "Failed unmarshal json response.")
		return nil, err
	}

	// Some registries set access_token instead of token.
	if response.AccessToken != "" {
		logger.Info("Setting gcr access token.")
		response.Token = response.AccessToken
	}

	// Find a token to turn into a Bearer authenticator
	if response.Token == "" {
		logger.Info("Failed, no gcr token in bearer response.")
		return nil, fmt.Errorf("no token in bearer response:\n%s", content)
	}

	logger.Info("Successfully authenticated with gcr.")
	// buildkit only supports username/password
	return &types.AuthConfig{
		Username: "oauth2accesstoken",
		Password: token.AccessToken,

		RegistryToken: response.Token,
	}, nil
}
