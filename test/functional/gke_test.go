//go:build functional && gke

package functional

import (
	"context"
	"os"
	"testing"

	"github.com/dominodatalab/testenv"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestGKEFunctionality(t *testing.T) {
	suite.Run(t, new(GKETestSuite))
}

type GKETestSuite struct {
	suite.Suite

	manager   testenv.Manager
	clientset kubernetes.Interface
}

func (suite *GKETestSuite) SetupSuite() {
	ctx := context.Background()
	cfg := testenv.GKEConfig{
		KubernetesVersion: os.Getenv("KUBERNETES_VERSION"),
		ProjectID:         os.Getenv("GCP_PROJECT_ID"),
		Region:            os.Getenv("GCP_REGION"),
	}

	var err error
	suite.manager, err = testenv.NewCloudEnvManager(ctx, cfg, true)
	require.NoError(suite.T(), err)
	require.NoError(suite.T(), suite.manager.Create(ctx))
	require.NoError(suite.T(), suite.manager.Apply(ctx, "helmfile.yaml"))

	configBytes, err := suite.manager.KubeconfigBytes(ctx)
	require.NoError(suite.T(), err)

	clientConfig, err := clientcmd.NewClientConfigFromBytes(configBytes)
	require.NoError(suite.T(), err)

	restConfig, err := clientConfig.ClientConfig()
	require.NoError(suite.T(), err)

	suite.clientset, err = kubernetes.NewForConfig(restConfig)
	require.NoError(suite.T(), err)
}

func (suite *GKETestSuite) TearDownSuite() {
	// TODO: teardown cluster
}

func (suite *GKETestSuite) TestLocalRegistry() {
	// TODO: implement
}
