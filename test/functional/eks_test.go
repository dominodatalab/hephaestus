//go:build functional && eks

package functional

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/testenv"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestEKSFunctionality(t *testing.T) {
	suite.Run(t, new(EKSTestSuite))
}

type EKSTestSuite struct {
	GenericImageBuilderTestSuite

	region string
}

func (suite *EKSTestSuite) SetupSuite() {
	suite.region = os.Getenv("AWS_REGION")
	suite.CloudAuthTest = suite.testCloudAuth
	suite.CloudConfigFunc = func() testenv.CloudConfig {
		return testenv.EKSConfig{
			Region:            suite.region,
			KubernetesVersion: os.Getenv("KUBERNETES_VERSION"),
		}
	}

	suite.GenericImageBuilderTestSuite.SetupSuite()
}

func (suite *EKSTestSuite) testCloudAuth(ctx context.Context, t *testing.T) {
	fullRepo, err := suite.manager.OutputVar(ctx, "repository")
	require.NoError(t, err)

	canonicalImage := string(fullRepo)
	cloudRegistry := strings.SplitN(canonicalImage, "/", 2)[0]

	build := newImageBuild(
		python39JupyterBuildContext,
		canonicalImage,
		&hephv1.RegistryCredentials{
			Server: cloudRegistry,
		},
	)
	ib := createBuild(t, ctx, suite.hephClient, build)

	conf, err := config.LoadDefaultConfig(ctx, config.WithEC2IMDSRegion())
	require.NoError(t, err)

	client := ecr.NewFromConfig(conf)
	input := &ecr.GetAuthorizationTokenInput{}
	resp, err := client.GetAuthorizationToken(ctx, input)
	require.NoError(t, err)

	authToken := aws.ToString(resp.AuthorizationData[0].AuthorizationToken)
	decoded, err := base64.StdEncoding.DecodeString(authToken)
	require.NoError(t, err)

	credentials := strings.SplitN(string(decoded), ":", 2)
	tags, err := crane.ListTags(
		canonicalImage,
		crane.WithContext(ctx),
		crane.WithAuth(newTestRegistryAuthenticator(
			credentials[0],
			credentials[1],
		)),
	)
	require.NoError(t, err)
	assert.Contains(t, tags, ib.Spec.LogKey)
}
