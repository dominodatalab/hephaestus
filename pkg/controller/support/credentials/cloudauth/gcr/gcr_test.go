package gcr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/oauth2"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
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

type roundTripFunc func(r *http.Request) (*http.Response, error)

func (s roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return s(r)
}

func TestAuthenticate(t *testing.T) {
	defaultCtx := context.Background()

	// Canceled context for testing failed do request
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// logger for comparing expected vs actual logging.
	observerCore, observedLogs := observer.New(zap.DebugLevel)
	log := zapr.NewLogger(zap.New(observerCore))

	// test server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "New Server, %s", r.Proto)
	}))
	ts.Close()

	// test server IP and port information for error logging
	tsAddress := ts.Listener.Addr()

	// expected errors
	invalidUrlErr := errors.New(fmt.Sprintf("invalid gcr url test-ts should match %s", gcrRegex))
	gcrTokenAccessErr := errors.New("unable to access gcr token from oauth: default error message")
	gcrRegistryErr := errors.New("GCR registry \"https://hi-docker.pkg.dev\" is unusable: default error message")
	ctxCanceledErr := errors.New(fmt.Sprintf("Get \"http://oauth2accesstoken:***@%s?client_id=forge&service=serviceName\": context canceled", tsAddress.String()))
	non200StatusErr := errors.New("failed to obtain token, received unexpected response code: 400\nresponse: \"nope\"")
	noResTokenErr := errors.New("failed, no gcr token in bearer response:\n{\"token\":\"\",\"access_token\":\"\",\"refresh_token\":\"\"}")

	for _, tt := range []struct {
		name                        string
		serverName                  string
		ctx                         context.Context
		roundTripper                roundTripFunc
		provider                    *gcrProvider
		authConfig                  *types.AuthConfig
		defaultChallengeLoginServer cloudauth.LoginChallenger
		expectedLogMessage          string
		expectedError               error
	}{
		{
			"invalid-url",
			"test-ts",
			defaultCtx,
			nil,
			&gcrProvider{
				logger:      log,
				tokenSource: nil,
			},
			nil,
			defaultChallengeLoginServer,
			invalidUrlErr.Error(),
			invalidUrlErr,
		},
		{
			"oauth2-error",
			"gcr.io",
			defaultCtx,
			nil,
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{errOut: true},
			},
			nil,
			defaultChallengeLoginServer,
			gcrTokenAccessErr.Error(),
			gcrTokenAccessErr,
		},
		{
			"failed-challenge-ts-error",
			"hi-docker.pkg.dev",
			defaultCtx,
			nil,
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				"realmName",
				defaultTestingErr),
			gcrRegistryErr.Error(),
			gcrRegistryErr,
		},
		{
			"failed-do-request",
			"gcr.io",
			canceledCtx,
			nil,
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil),
			ctxCanceledErr.Error(),
			ctxCanceledErr,
		},
		{
			"non-200-response-code",
			"gcr.io",
			defaultCtx,
			roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`nope`)),
				}, nil
			}),
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil),
			non200StatusErr.Error(),
			non200StatusErr,
		},
		{
			"failed-no-token-in-response",
			"gcr.io",
			defaultCtx,
			roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := json.Marshal(tokenResponse{})
				if err != nil {
					t.Fatalf("Unexpected error while marshalling token response")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(string(res))),
				}, nil
			}),
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil),
			noResTokenErr.Error(),
			noResTokenErr,
		},
		// success
		{
			"token-response-has-access-token",
			"gcr.io",
			defaultCtx,
			roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := json.Marshal(tokenResponse{
					AccessToken: "test-access-token",
				})
				if err != nil {
					t.Fatalf("Unexpected error while marshalling token response")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(string(res))),
				}, nil
			}),
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			&types.AuthConfig{
				Username:      "oauth2accesstoken",
				Password:      "hey",
				RegistryToken: "test-access-token",
			},
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil),
			"Successfully authenticated with gcr \"gcr.io\"",
			nil,
		},
		{
			"token-response",
			"gcr.io",
			defaultCtx,
			roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := json.Marshal(tokenResponse{
					Token: "regular-token",
				})
				if err != nil {
					t.Fatalf("Unexpected error while marshalling token response")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(string(res))),
				}, nil
			}),
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			&types.AuthConfig{
				Username:      "oauth2accesstoken",
				Password:      "hey",
				RegistryToken: "regular-token",
			},
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil),
			"Successfully authenticated with gcr \"gcr.io\"",
			nil,
		},
		{
			"refresh-token-response",
			"gcr.io",
			defaultCtx,
			roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := json.Marshal(tokenResponse{
					Token:        "regular-token",
					RefreshToken: "ignore-this-refresh-token",
				})
				if err != nil {
					t.Fatalf("Unexpected error while marshalling token response")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(string(res))),
				}, nil
			}),
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			&types.AuthConfig{
				Username:      "oauth2accesstoken",
				Password:      "hey",
				RegistryToken: "regular-token",
			},
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil),
			"Successfully authenticated with gcr \"gcr.io\"",
			nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defaultChallengeLoginServer = tt.defaultChallengeLoginServer
			defaultClient = ts.Client()

			if tt.roundTripper != nil {
				defaultClient.Transport = tt.roundTripper
			}

			authConfig, err := tt.provider.authenticate(tt.ctx, log, tt.serverName)
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
