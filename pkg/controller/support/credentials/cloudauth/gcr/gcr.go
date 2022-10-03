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

func (g *gcrProvider) authenticate(ctx context.Context, logger logr.Logger, server string) (*types.AuthConfig, error) {
	match := gcrRegex.FindAllString(server, -1)
	if len(match) != 1 {
		err := fmt.Errorf(fmt.Sprintf("Invalid gcr url %s should match %s", server, gcrRegex))
		logger.Info(err.Error())

		return nil, fmt.Errorf("invalid gcr url: %q should match %v", server, gcrRegex)
	}

	token, err := g.tokenSource.Token()
	if err != nil {
		err = fmt.Errorf("unable to access gcr token from oauth: %w", err)
		logger.Info(err.Error())

		return nil, err
	}

	loginServerURL := "https://" + match[0]
	directive, err := defaultChallengeLoginServer(ctx, loginServerURL)
	if err != nil {
		err = fmt.Errorf("GCR registry %q is unusable: %w", loginServerURL, err)
		logger.Info(err.Error())

		return nil, err
	}

	// obtain the registry token
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directive.Realm, nil)
	if err != nil {
		err = fmt.Errorf("unable to create new request: %w", err)
		logger.Info(err.Error())

		return nil, err
	}

	v := url.Values{}
	v.Set("service", directive.Service)
	v.Set("client_id", "forge")
	req.URL.RawQuery = v.Encode()
	req.URL.User = url.UserPassword("oauth2accesstoken", token.AccessToken)
	resp, err := defaultClient.Do(req)
	if err != nil {
		err = fmt.Errorf("unable to make a request to url %q\nerror: %w", req.URL, err)
		logger.Info(err.Error())

		return nil, err
	}

	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("unable to read response body %w", err)
		logger.Info(err.Error())

		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("failed to obtain token, received unexpected response code: %d\nresponse: %q",
			resp.StatusCode, content)
		logger.Info(err.Error())

		return nil, err
	}

	var response tokenResponse
	if err = json.Unmarshal(content, &response); err != nil {
		err = fmt.Errorf("failed unmarshal json token response: %w", err)
		return nil, err
	}

	// Some registries set access_token instead of token.
	if response.AccessToken != "" {
		logger.Info("Setting gcr access token.")
		response.Token = response.AccessToken
	}

	// Find a token to turn into a Bearer authenticator
	if response.Token == "" {
		err = fmt.Errorf("failed, no gcr token in bearer response:\n%s", content)
		logger.Info(err.Error())

		return nil, err
	}

	logger.Info(fmt.Sprintf("Successfully authenticated with gcr %q", server))
	// buildkit only supports username/password
	return &types.AuthConfig{
		Username:      "oauth2accesstoken",
		Password:      token.AccessToken,
		RegistryToken: response.Token,
	}, nil
}
