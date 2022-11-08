//go:build functional && gke

package functional

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/dominodatalab/testenv"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
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

	suite.CloudConfigFunc = func() testenv.CloudConfig {
		return testenv.GKEConfig{
			Region:                   suite.region,
			ProjectID:                suite.projectID,
			KubernetesVersion:        os.Getenv("KUBERNETES_VERSION"),
			KubernetesServiceAccount: "default/hephaestus",
		}
	}
	suite.VariableFunc = func(ctx context.Context) {
		repoName, err := suite.manager.OutputVar(ctx, "repository")
		require.NoError(suite.T(), err)

		suite.cloudRegistry = fmt.Sprintf("%s-docker.pkg.dev", suite.region)
		suite.cloudRepository = fmt.Sprintf("%s/%s", suite.projectID, repoName)

		gcpServiceAccount, err := suite.manager.OutputVar(ctx, "service_account")
		require.NoError(suite.T(), err)

		suite.helmfileValues = []string{
			"controller.manager.cloudRegistryAuth.gcp.enabled=true",
			fmt.Sprintf("controller.manager.cloudRegistryAuth.gcp.serviceAccount=%s", gcpServiceAccount),
		}
	}

	suite.GenericImageBuilderTestSuite.SetupSuite()
}
