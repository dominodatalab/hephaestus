//go:build functional && aks

package functional

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/runtime/2019-08-15-preview/containerregistry"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/testenv"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"k8s.io/utils/pointer"
)

func TestAKSFunctionality(t *testing.T) {
	suite.Run(t, new(AKSTestSuite))
}

type AKSTestSuite struct {
	GenericImageBuilderTestSuite
}

func (suite *AKSTestSuite) SetupSuite() {
	suite.CloudAuthTest = suite.testCloudAuth
	suite.CloudConfigFunc = func() testenv.CloudConfig {
		return testenv.AKSConfig{
			Location:          os.Getenv("AZURE_LOCATION"),
			KubernetesVersion: os.Getenv("KUBERNETES_VERSION"),
		}
	}
	suite.VariableFunc = func(ctx context.Context) {
		tenantID, err := suite.manager.OutputVar(ctx, "tenant_id")
		suite.Require().NoError(err)

		clientID, err := suite.manager.OutputVar(ctx, "client_id")
		suite.Require().NoError(err)

		clientSecret, err := suite.manager.OutputVar(ctx, "client_secret")
		suite.Require().NoError(err)

		suite.helmfileValues = []string{
			"controller.manager.cloudRegistryAuth.azure.enabled=true",
			fmt.Sprintf("controller.manager.cloudRegistryAuth.azure.tenantID=%s", tenantID),
			fmt.Sprintf("controller.manager.cloudRegistryAuth.azure.clientID=%s", clientID),
			fmt.Sprintf("controller.manager.cloudRegistryAuth.azure.clientSecret=%s", clientSecret),
		}
	}
	suite.GenericImageBuilderTestSuite.SetupSuite()
}

func (suite *AKSTestSuite) testCloudAuth(ctx context.Context, t *testing.T) {
	repoName, err := suite.manager.OutputVar(ctx, "repository")
	require.NoError(t, err)

	tid, err := suite.manager.OutputVar(ctx, "tenant_id")
	require.NoError(t, err)
	tenantID := string(tid)

	cloudRegistry := string(repoName)
	cloudRepository := "test-image"
	image := fmt.Sprintf("%s/%s", cloudRegistry, cloudRepository)
	build := newImageBuild(
		python39JupyterBuildContext,
		image,
		&hephv1.RegistryCredentials{
			Server:        cloudRegistry,
			CloudProvided: pointer.Bool(true),
		},
	)
	ib := createBuild(t, ctx, suite.hephClient, build)

	cred, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
		TenantID: tenantID,
	})
	require.NoError(t, err)

	aadToken, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{azure.PublicCloud.TokenAudience},
	})
	require.NoError(t, err)

	registryURL := fmt.Sprintf("https://%s", cloudRegistry)
	rtClient := containerregistry.NewRefreshTokensClient(registryURL)
	refreshToken, err := rtClient.GetFromExchange(
		ctx,
		"access_token",
		cloudRegistry,
		tenantID,
		"",
		aadToken.Token,
	)
	require.NoError(t, err)

	tags, err := crane.ListTags(
		image,
		crane.WithContext(ctx),
		crane.WithAuth(newTestRegistryAuthenticator(
			"00000000-0000-0000-0000-000000000000",
			to.String(refreshToken.RefreshToken),
		)),
	)
	require.NoError(t, err)
	assert.Contains(t, tags, ib.Spec.LogKey)
}
