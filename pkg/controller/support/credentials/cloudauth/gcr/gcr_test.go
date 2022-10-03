package gcr

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth/cloudauthtest"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/oauth2"
	"testing"
)

var (
	defaultTestingErr = errors.New("seals are sea puppies")
)

type fakeOauth2TokenSource struct {
	errOut bool
}

func (f *fakeOauth2TokenSource) Token() (*oauth2.Token, error) {
	if f.errOut {
		return nil, defaultTestingErr
	}
	return nil, nil
}

func TestAuthenticate(t *testing.T) {
	invalidServerError := errors.New("invalid gcr url: \"test-server\" should match .*-docker\\.pkg\\.dev|(?:.*\\.)?gcr\\.io")

	ctx := context.Background()
	observerCore, observedLogs := observer.New(zap.DebugLevel)
	log := zapr.NewLogger(zap.New(observerCore))

	for _, tt := range []struct {
		name                        string
		serverName                  string
		provider                    *gcrProvider
		authConfig                  *types.AuthConfig
		defaultChallengeLoginServer func(ctx context.Context, loginServerURL string) (*cloudauth.AuthDirective, error)
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
			invalidServerError,
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
			"Unable to access gcr token.",
			defaultTestingErr,
		},
		{
			"failed-challenge-server-error",
			"gcr.io",
			&gcrProvider{
				logger:      log,
				tokenSource: &fakeOauth2TokenSource{},
			},
			nil,
			cloudauthtest.FakeChallengeLoginServer(
				"serviceName",
				"realmName",
				defaultTestingErr),
			"Failed gcr cloud authentication.",
			defaultTestingErr,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defaultChallengeLoginServer = tt.defaultChallengeLoginServer

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
