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
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/smithy-go/logging"
	"github.com/docker/docker/api/types/registry"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
	"github.com/go-logr/logr"
)

type ecrClient interface {
	GetAuthorizationToken(
		ctx context.Context,
		params *ecr.GetAuthorizationTokenInput,
		optFns ...func(*ecr.Options),
	) (*ecr.GetAuthorizationTokenOutput, error)
}

type awsLogger struct {
	logr.Logger
}

func (l awsLogger) Logf(classification logging.Classification, format string, v ...interface{}) {
	var level int
	if classification == logging.Debug {
		level = 1
	}

	l.V(level).Info(fmt.Sprintf(format, v...))
}

var (
	client   ecrClient
	urlRegex = regexp.MustCompile(
		`^(?P<aws_account_id>[a-zA-Z\d][a-zA-Z\d-_]*)\.dkr\.ecr(-fips)?\.([a-zA-Z\d][a-zA-Z\d-_]*)\.amazonaws\.com(\.cn)?`,
	)
)

func Register(ctx context.Context, logger logr.Logger, registry *cloudauth.Registry) error {
	clientMode := aws.LogRequest | aws.LogResponse | aws.LogRetries
	clientLogger := &awsLogger{logger}
	awsConfig, err := config.LoadDefaultConfig(
		ctx,
		config.WithEC2IMDSRegion(func(o *config.UseEC2IMDSRegion) {
			o.Client = imds.New(imds.Options{
				ClientLogMode: clientMode,
				Logger:        clientLogger,
			})
		}),
		config.WithLogger(clientLogger),
		config.WithClientLogMode(clientMode),
	)
	if err != nil {
		logger.Info("ECR not registered", "error", err)
		return nil
	}

	client = ecr.NewFromConfig(awsConfig)

	registry.Register(urlRegex, authenticate)
	logger.Info("ECR registered")
	return nil
}

func authenticate(ctx context.Context, logger logr.Logger, url string) (*registry.AuthConfig, error) {
	logger.WithName("ecr-auth-provider")

	if !urlRegex.MatchString(url) {
		err := fmt.Errorf("ECR URL is invalid: %q should match pattern %v", url, urlRegex)
		logger.Info(err.Error())
		return nil, err
	}
	input := &ecr.GetAuthorizationTokenInput{}

	resp, err := client.GetAuthorizationToken(ctx, input)
	if err != nil {
		err = fmt.Errorf("failed to access ECR auth token: %w", err)
		logger.Info(err.Error())
		return nil, err
	}
	if len(resp.AuthorizationData) != 1 {
		err = fmt.Errorf("expected a single ECR authorization token: %v", resp.AuthorizationData)
		logger.Info(err.Error())
		return nil, err
	}
	authToken := aws.ToString(resp.AuthorizationData[0].AuthorizationToken)

	username, password, err := decodeAuth(authToken)
	if err != nil {
		err = fmt.Errorf("invalid ECR authorization token: %w", err)
		logger.Info(err.Error())
		return nil, err
	}

	logger.Info("Successfully authenticated with ECR")
	return &registry.AuthConfig{
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
