package ecr

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/docker/docker/api/types"
	"github.com/go-logr/logr"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

type ecrClient interface {
	GetAuthorizationToken(
		ctx context.Context,
		params *ecr.GetAuthorizationTokenInput,
		optFns ...func(*ecr.Options),
	) (*ecr.GetAuthorizationTokenOutput, error)
}

var (
	client   ecrClient
	urlRegex = regexp.MustCompile(
		`^(?P<aws_account_id>[a-zA-Z\d][a-zA-Z\d-_]*)\.dkr\.ecr(-fips)?\.([a-zA-Z\d][a-zA-Z\d-_]*)\.amazonaws\.com(\.cn)?`,
	)
)

func Register(ctx context.Context, logger logr.Logger, registry *cloudauth.Registry) error {
	config, err := config.LoadDefaultConfig(ctx, config.WithEC2IMDSRegion())
	if err != nil {
		logger.Info("ECR not registered", "error", err)
		return nil
	}

	client = ecr.NewFromConfig(config)

	registry.Register(urlRegex, authenticate)
	logger.Info("ECR registered")
	return nil
}

func authenticate(ctx context.Context, logger logr.Logger, url string) (*types.AuthConfig, error) {
	logger.WithName("ecr-auth-provider")

	if !urlRegex.MatchString(url) {
		logger.V(2).Info(fmt.Sprintf("Invalid ecr url. %s should match %s", url, urlRegex))
		return nil, fmt.Errorf("invalid ecr url: %q should match %v", url, urlRegex)
	}
	input := &ecr.GetAuthorizationTokenInput{}

	resp, err := client.GetAuthorizationToken(ctx, input)
	if err != nil {
		logger.Error(err, "Failed to access ecr auth token.")
		return nil, fmt.Errorf("failed to get ecr auth token: %w", err)
	}
	if len(resp.AuthorizationData) != 1 {
		logger.Info(fmt.Sprintf("Expected a single ecr token, received: %v", resp.AuthorizationData))
		return nil, fmt.Errorf("expected a single ecr authorization token: %v", resp.AuthorizationData)
	}
	authToken := aws.ToString(resp.AuthorizationData[0].AuthorizationToken)

	username, password, err := decodeAuth(authToken)
	if err != nil {
		logger.Error(err, "Invalid ecr authorization token.")
		return nil, fmt.Errorf("invalid ecr authorization token: %w", err)
	}

	logger.Info("Successfully authenticated with ecr.")
	return &types.AuthConfig{
		Username: username,
		Password: password,
	}, nil
}

func decodeAuth(auth string) (string, string, error) {
	if auth == "" {
		return "", "", errors.New("docker auth token cannot be blank")
	}

	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode docker auth token: %w", err)
	}

	creds := strings.SplitN(string(decoded), ":", 2)
	if len(creds) != 2 {
		return "", "", fmt.Errorf("invalid docker auth token: %q", creds)
	}
	return creds[0], creds[1], nil
}
