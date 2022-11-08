//go:build functional && eks

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

func TestEKSFunctionality(t *testing.T) {
	suite.Run(t, new(EKSTestSuite))
}

type EKSTestSuite struct {
	GenericImageBuilderTestSuite
	region string
}

func (suite *EKSTestSuite) SetupSuite() {
	suite.region = os.Getenv("AWS_REGION")
	suite.CloudConfigFunc = func() testenv.CloudConfig {
		return testenv.EKSConfig{
			Region:            suite.region,
			KubernetesVersion: os.Getenv("KUBERNETES_VERSION"),
		}
	}

	suite.VariableFunc = func(ctx context.Context) {
		repoName, err := suite.manager.OutputVar(ctx, "repository")
		require.NoError(suite.T(), err)

		suite.cloudRegistry = fmt.Sprintf("%s-docker.pkg.dev", suite.region)
		suite.cloudRepository = string(repoName)
	}
	suite.GenericImageBuilderTestSuite.SetupSuite()
}
