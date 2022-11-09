//go:build functional && eks

package functional

import (
	"context"
	"os"
	"strings"
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
		repoUrl, err := suite.manager.OutputVar(ctx, "repository")
		require.NoError(suite.T(), err)

		repoS := strings.Split(string(repoUrl), "/")
		require.Equal(suite.T(), 2, len(repoS))
		suite.cloudRegistry, suite.cloudRepository = repoS[0], repoS[1]
	}
	suite.GenericImageBuilderTestSuite.SetupSuite()
}
