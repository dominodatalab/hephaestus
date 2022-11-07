//go:build functional && eks

package functional

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dominodatalab/hephaestus/pkg/clientset"
	"github.com/dominodatalab/testenv"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestEKSFunctionality(t *testing.T) {
	suite.Run(t, new(EKSTestSuite))
}

type EKSTestSuite struct {
	suite.Suite

	suiteSetupDone bool
	eksRepository  string

	manager    testenv.Manager
	k8sClient  kubernetes.Interface
	hephClient clientset.Interface
}

func (suite *EKSTestSuite) SetupSuite() {
	verbose := os.Getenv("VERBOSE_TESTING") == "true"
	region := os.Getenv("AWS_REGION")
	k8sVersion := os.Getenv("KUBERNETES_VERSION")
	managerImageTag := os.Getenv("MANAGER_IMAGE_TAG")

	cfg := testenv.EKSConfig{
		Region:            region,
		KubernetesVersion: k8sVersion,
	}

	var err error
	ctx := context.Background()

	suite.manager, err = testenv.NewCloudEnvManager(ctx, cfg, verbose)
	require.NoError(suite.T(), err)
	defer func() {
		if !suite.suiteSetupDone {
			suite.TearDownSuite()
		}
	}()

	suite.T().Log("Creating EKS test environment")
	start := time.Now()
	require.NoError(suite.T(), suite.manager.Create(ctx))
	suite.T().Logf("Total cluster creation time: %s", time.Since(start))

	repoName, err := suite.manager.OutputVar(ctx, "repository")
	require.NoError(suite.T(), err)

	suite.eksRepository = string(repoName)

	helmfileValues := []string{
		fmt.Sprintf("controller.manager.image.tag=%s", managerImageTag),
	}

	suite.T().Log("Installing cluster applications")
	start = time.Now()
	require.NoError(suite.T(), suite.manager.HelmfileApply(ctx, "helmfile.yaml", helmfileValues))
	suite.T().Logf("Total application install time: %s", time.Since(start))

	configBytes, err := suite.manager.KubeconfigBytes(ctx)
	require.NoError(suite.T(), err)

	clientConfig, err := clientcmd.NewClientConfigFromBytes(configBytes)
	require.NoError(suite.T(), err)

	restConfig, err := clientConfig.ClientConfig()
	require.NoError(suite.T(), err)

	suite.k8sClient, err = kubernetes.NewForConfig(restConfig)
	require.NoError(suite.T(), err)

	suite.hephClient, err = clientset.NewForConfig(restConfig)
	require.NoError(suite.T(), err)

	suite.T().Log("Test setup complete")
	suite.suiteSetupDone = true
}

func (suite *EKSTestSuite) TearDownSuite() {
	suite.T().Log("Tearing down EKS test cluster")
	require.NoError(suite.T(), suite.manager.Destroy(context.Background()))
}
