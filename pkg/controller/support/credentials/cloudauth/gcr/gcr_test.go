package gcr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/oauth2"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/cloudauthtest"
)

var (
	defaultTestingErr = errors.New("default error message")
)

type fakeOauth2TokenSource struct {
	errOut bool
}

func (f *fakeOauth2TokenSource) Token() (*oauth2.Token, error) {
	if f.errOut {
		return nil, defaultTestingErr
	}
	return &oauth2.Token{
		AccessToken: "hey",
	}, nil
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	observerCore, observedLogs := observer.New(zap.DebugLevel)
	log := zapr.NewLogger(zap.New(observerCore))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "New Server, %s", r.Proto)
	}))
	server.Close()

	for _, tt := range []struct {
		name                        string
		serverName                  string
		provider                    *gcrProvider
		authConfig                  *types.AuthConfig
		defaultChallengeLoginServer cloudauthtest.LoginChallenger
		expectedLogMessage          string
		expectedError               error
	}{
		{
			"invalid-url",
			"test-server",
			&gcrProvider{
				logger:      log,
				tokenSource: nil,
			},
			nil,
			defaultChallengeLoginServer,
			fmt.Sprintf("Invalid gcr url test-server should match %s", gcrRegex),
			errors.New("invalid gcr url: \"test-server\" should match .*-docker\\.pkg\\.dev|(?:.*\\.)?gcr\\.io"),
		},
		{
			"oauth2-error",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{errOut: true},
			},
			nil,
			defaultChallengeLoginServer,
			"unable to access gcr token from oauth: default error message",
			errors.New("unable to access gcr token from oauth: default error message"),
		},
		{
			"failed-challenge-server-error",
			"hi-docker.pkg.dev",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				"realmName",
				defaultTestingErr),
			"GCR registry \"https://hi-docker.pkg.dev\" is unusable: default error message",
			errors.New("GCR registry \"https://hi-docker.pkg.dev\" is unusable: default error message"),
		},

		// TODO: http errors - WIP
		{
			"failed-create-request",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		{
			"failed-do-request",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		{
			"invalid-response-body",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		{
			"non-200-response-code",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		{
			"failed-to-unmarshal-response-body",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		{
			"setting-response-access-token",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		{
			"failed-no-token-in-response",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
		// success
		{
			"successfully-authenticated-with-gcr",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				server.URL,
				nil),
			"",
			errors.New(""),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defaultChallengeLoginServer = tt.defaultChallengeLoginServer

			defaultClient = server.Client()

			authConfig, err := tt.provider.authenticate(ctx, log, tt.serverName)
			assert.Equal(t, tt.authConfig, authConfig)

			if tt.expectedError != nil {
				require.Error(t, err)
				assert.Equal(t, err.Error(), tt.expectedError.Error())
			} else {
				require.NoError(t, err)
			}

			logLen := observedLogs.Len()
			require.GreaterOrEqual(t, logLen, 1)
			recentLogMessage := observedLogs.All()[logLen-1].Message
			assert.Equal(t, tt.expectedLogMessage, recentLogMessage)
		})
	}
}
