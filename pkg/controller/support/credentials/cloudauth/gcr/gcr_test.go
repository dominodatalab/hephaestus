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

	"github.com/docker/docker/api/types/registry"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/oauth2"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/cloudauthtest"
)

var defaultTestingErr = errors.New("default error message")

func TestAuthenticate(t *testing.T) {
	defaultCtx := context.Background()

	// logger for comparing expected vs actual logging.
	observerCore, observedLogs := observer.New(zap.DebugLevel)
	log := zapr.NewLogger(zap.New(observerCore))

	// test server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "New Server, %s", r.Proto)
	}))

	t.Cleanup(ts.Close)

	// expected errors
	invalidUrlErr := errors.New(fmt.Sprintf("invalid GCR URL test-ts should match %s", gcrRegex))
	gcrTokenAccessErr := errors.New("unable to access GCR token from oauth: default error message")

	for _, tt := range []struct {
		name               string
		serverName         string
		ctx                context.Context
		roundTripper       roundTripFunc
		tokenShouldErr     bool
		authConfig         *registry.AuthConfig
		loginChallenger    cloudauth.LoginChallenger
		expectedLogMessage string
		expectedError      error
	}{
		{
			name:               "invalid_url",
			serverName:         "test-ts",
			ctx:                defaultCtx,
			roundTripper:       nil,
			authConfig:         nil,
			loginChallenger:    defaultChallengeLoginServer,
			expectedLogMessage: invalidUrlErr.Error(),
			expectedError:      invalidUrlErr,
		},
		{
			name:               "oauth2_error",
			serverName:         "gcr.io",
			ctx:                defaultCtx,
			tokenShouldErr:     true,
			loginChallenger:    defaultChallengeLoginServer,
			expectedLogMessage: gcrTokenAccessErr.Error(),
			expectedError:      gcrTokenAccessErr,
		},
		// success
		{
			name:         "token_response_has_access_token",
			serverName:   "gcr.io",
			ctx:          defaultCtx,
			roundTripper: createRoundTripperFunc(t, tokenResponse{AccessToken: "test-access-token"}, http.StatusOK),
			authConfig: &registry.AuthConfig{
				Username:      "oauth2accesstoken",
				Password:      "hey",
				RegistryToken: "",
			},
			loginChallenger: cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil,
			),
			expectedLogMessage: "Successfully authenticated with GCR \"gcr.io\"",
		},
		{
			name:         "token_response",
			serverName:   "gcr.io",
			ctx:          defaultCtx,
			roundTripper: createRoundTripperFunc(t, tokenResponse{Token: "regular-token"}, http.StatusOK),
			authConfig: &registry.AuthConfig{
				Username:      "oauth2accesstoken",
				Password:      "hey",
				RegistryToken: "",
			},
			loginChallenger: cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil,
			),
			expectedLogMessage: "Successfully authenticated with GCR \"gcr.io\"",
		},
		{
			name:       "refresh_token_response",
			serverName: "gcr.io",
			ctx:        defaultCtx,
			roundTripper: createRoundTripperFunc(t, tokenResponse{
				Token: "regular-token", RefreshToken: "ignore-this-refresh-token",
			}, http.StatusOK),
			authConfig: &registry.AuthConfig{
				Username:      "oauth2accesstoken",
				Password:      "hey",
				RegistryToken: "",
			},
			loginChallenger: cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				ts.URL,
				nil,
			),
			expectedLogMessage: "Successfully authenticated with GCR \"gcr.io\"",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defaultChallengeLoginServer = tt.loginChallenger
			defaultClient = ts.Client()

			if tt.roundTripper != nil {
				defaultClient.Transport = tt.roundTripper
			}

			provider := &gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{errOut: tt.tokenShouldErr},
			}

			authConfig, err := provider.authenticate(tt.ctx, log, tt.serverName)
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

func createRoundTripperFunc(t *testing.T, tr tokenResponse, statusCode int) roundTripFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		res, err := json.Marshal(tr)
		if err != nil {
			t.Fatalf("Unexpected error while marshalling token response. %#v", err)
		}
		return &http.Response{
			StatusCode: statusCode,
			Body:       io.NopCloser(strings.NewReader(string(res))),
		}, nil
	}
}
