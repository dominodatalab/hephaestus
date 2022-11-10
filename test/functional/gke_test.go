//go:build functional && gke

package functional

import (
	"context"
	"fmt"
	"os"
	"testing"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/testenv"
	"github.com/heroku/docker-registry-client/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	auth "golang.org/x/oauth2/google"
	"k8s.io/utils/pointer"
)

func TestGKEFunctionality(t *testing.T) {
	suite.Run(t, new(GKETestSuite))
}

type GKETestSuite struct {
	GenericImageBuilderTestSuite

	region    string
	projectID string
}

func (suite *GKETestSuite) SetupSuite() {
	suite.region = os.Getenv("GCP_REGION")
	suite.projectID = os.Getenv("GCP_PROJECT_ID")
	suite.CloudAuthTest = suite.testCloudAuth
	suite.CloudConfigFunc = func() testenv.CloudConfig {
		return testenv.GKEConfig{
			Region:                   suite.region,
			ProjectID:                suite.projectID,
			KubernetesVersion:        os.Getenv("KUBERNETES_VERSION"),
			KubernetesServiceAccount: "default/hephaestus",
		}
	}
	suite.VariableFunc = func(ctx context.Context) {
		gcpServiceAccount, err := suite.manager.OutputVar(ctx, "service_account")
		require.NoError(suite.T(), err)

		suite.helmfileValues = []string{
			"controller.manager.cloudRegistryAuth.gcp.enabled=true",
			fmt.Sprintf("controller.manager.cloudRegistryAuth.gcp.serviceAccount=%s", gcpServiceAccount),
		}
	}

	suite.GenericImageBuilderTestSuite.SetupSuite()
}

func (suite *GKETestSuite) testCloudAuth(ctx context.Context, t *testing.T) {
	repoName, err := suite.manager.OutputVar(ctx, "repository")
	require.NoError(suite.T(), err)

	cloudRegistry := fmt.Sprintf("%s-docker.pkg.dev", suite.region)
	cloudRepository := fmt.Sprintf("%s/%s", suite.projectID, repoName)

	image := fmt.Sprintf("%s/test-image", cloudRepository)
	build := newImageBuild(
		python39JupyterBuildContext,
		fmt.Sprintf("%s/%s", cloudRegistry, image),
		&hephv1.RegistryCredentials{
			Server:        cloudRegistry,
			CloudProvided: pointer.Bool(true),
		},
	)
	ib := createBuild(t, ctx, suite.hephClient, build)

	credentials, err := auth.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	require.NoError(t, err)

	token, err := credentials.TokenSource.Token()
	require.NoError(t, err)

	hub, err := registry.New(fmt.Sprintf("https://%s", cloudRegistry), "oauth2accesstoken", token.AccessToken)
	require.NoError(t, err)

	tags, err := hub.Tags(image)
	require.NoError(t, err)
	assert.Contains(t, tags, ib.Spec.LogKey)
}
