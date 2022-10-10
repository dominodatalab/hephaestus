package ecr

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAuthenticate(t *testing.T) {
	defaultCtx := context.Background()
	observerCore, observedLogs := observer.New(zap.DebugLevel)
	log := zapr.NewLogger(zap.New(observerCore))

	var emptyToken *string
	invalidB64 := "%"
	invalidToken := "YWJk"
	validToken := "YWJjOmhp"

	// expected errors
	invalidServerErr := fmt.Errorf("ECR URL is invalid: \"0123456789012.dkr.ecr.us-west-2.amazonaws.io\" should match pattern %v", urlRegex)
	TokenAccessErr := fmt.Errorf("failed to access ECR auth token: test error")
	failedAuthTokenErr := errors.New("expected a single ECR authorization token: []")
	emptyAuthTokenErr := errors.New("invalid ECR authorization token: docker auth token cannot be blank")
	invalidB64Err := errors.New("invalid ECR authorization token: failed to decode docker auth token: illegal base64 data at input byte 0")
	invalidTokenErr := errors.New("invalid ECR authorization token: invalid docker auth token: [\"abd\"]")

	successMsg := "Successfully authenticated with ECR"

	for _, tt := range []struct {
		name               string
		serverUrl          string
		client             fakeECRClient
		authConfig         *dockerTypes.AuthConfig
		expectedLogMessage string
		expectedError      error
	}{
		{
			name:               "invalid_server",
			serverUrl:          "0123456789012.dkr.ecr.us-west-2.amazonaws.io",
			expectedLogMessage: invalidServerErr.Error(),
			expectedError:      invalidServerErr,
		},
		{
			name:               "errored_authorization_token",
			serverUrl:          "0123456789012.dkr.ecr.us-west-2.amazonaws.com",
			client:             fakeECRClient{errOut: true},
			expectedLogMessage: TokenAccessErr.Error(),
			expectedError:      TokenAccessErr,
		},
		{
			name:      "failed_authorization_data",
			serverUrl: "0123456789012.dkr.ecr.us-west-2.amazonaws.com",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{},
				},
			},
			expectedLogMessage: failedAuthTokenErr.Error(),
			expectedError:      failedAuthTokenErr,
		},
		{
			name:      "invalid_ecr_auth_token",
			serverUrl: "0123456789012.dkr.ecr.us-west-2.amazonaws.com",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{
						{AuthorizationToken: emptyToken},
					},
				},
			},
			expectedLogMessage: emptyAuthTokenErr.Error(),
			expectedError:      emptyAuthTokenErr,
		},
		{
			name:      "invalid_ecr_auth_token_base_64",
			serverUrl: "0123456789012.dkr.ecr.us-west-2.amazonaws.com",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{
						{AuthorizationToken: &invalidB64},
					},
				},
			},
			expectedLogMessage: invalidB64Err.Error(),
			expectedError:      invalidB64Err,
		},
		{
			name:      "invalid_ecr_auth_token_len_err",
			serverUrl: "0123456789012.dkr.ecr.us-west-2.amazonaws.com",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{
						{AuthorizationToken: &invalidToken},
					},
				},
			},
			expectedLogMessage: invalidTokenErr.Error(),
			expectedError:      invalidTokenErr,
		},
		// success
		{
			name:      "success_server.com",
			serverUrl: "0123456789012.dkr.ecr.us-west-2.amazonaws.com",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{
						{AuthorizationToken: &validToken},
					},
				},
			},
			authConfig: &dockerTypes.AuthConfig{
				Username: "abc",
				Password: "hi",
			},
			expectedLogMessage: successMsg,
		},
		{
			name:      "success_server.com.cn",
			serverUrl: "0123456789012.dkr.ecr.us-west-2.amazonaws.com.cn",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{
						{AuthorizationToken: &validToken},
					},
				},
			},
			authConfig: &dockerTypes.AuthConfig{
				Username: "abc",
				Password: "hi",
			},
			expectedLogMessage: successMsg,
		},
		{
			name:      "success_server.fips",
			serverUrl: "0123456789012.dkr.ecr-fips.us-west-2.amazonaws.com",
			client: fakeECRClient{
				TokenOutput: &ecr.GetAuthorizationTokenOutput{
					AuthorizationData: []ecrTypes.AuthorizationData{
						{AuthorizationToken: &validToken},
					},
				},
			},
			authConfig: &dockerTypes.AuthConfig{
				Username: "abc",
				Password: "hi",
			},
			expectedLogMessage: successMsg,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client = &tt.client
			authConfig, err := authenticate(defaultCtx, log, tt.serverUrl)

			// Compare expected error condition.
			if tt.expectedError == nil {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.expectedError.Error(), err.Error())
			}

			reflect.DeepEqual(tt.authConfig, authConfig)

			// Compare expected log messages.
			logLen := observedLogs.Len()
			require.GreaterOrEqual(t, logLen, 1)
			recentLogMessage := observedLogs.All()[observedLogs.Len()-1].Message
			assert.Equal(t, tt.expectedLogMessage, recentLogMessage)
		})
	}
}

type fakeECRClient struct {
	errOut      bool
	TokenOutput *ecr.GetAuthorizationTokenOutput
}

func (f *fakeECRClient) GetAuthorizationToken(
	context.Context,
	*ecr.GetAuthorizationTokenInput,
	...func(*ecr.Options),
) (*ecr.GetAuthorizationTokenOutput, error) {
	if f.errOut {
		return nil, errors.New("test error")
	}
	return f.TokenOutput, nil
}
