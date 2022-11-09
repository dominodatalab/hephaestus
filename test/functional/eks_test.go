//go:build functional && eks

package functional

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
		repository, err := suite.manager.OutputVar(ctx, "repository")
		require.NoError(suite.T(), err)

		url, err := url.Parse(fmt.Sprintf("https://%s", string(repository)))
		require.NoError(suite.T(), err)
		suite.cloudRegistry, suite.cloudRepository = url.Host, filepath.Base(url.Path)
	}
	suite.GenericImageBuilderTestSuite.SetupSuite()
}
