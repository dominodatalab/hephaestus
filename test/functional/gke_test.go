//go:build functional && gke

package functional

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/clientset"
	"github.com/dominodatalab/testenv"
	"github.com/heroku/docker-registry-client/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestGKEFunctionality(t *testing.T) {
	suite.Run(t, new(GKETestSuite))
}

type GKETestSuite struct {
	suite.Suite

	manager    testenv.Manager
	k8sClient  kubernetes.Interface
	hephClient clientset.Interface
}

func (suite *GKETestSuite) SetupSuite() {
	verbose := os.Getenv("VERBOSE_TESTING") == "true"

	ctx := context.Background()
	cfg := testenv.GKEConfig{
		KubernetesVersion: os.Getenv("KUBERNETES_VERSION"),
		ProjectID:         os.Getenv("GCP_PROJECT_ID"),
		Region:            os.Getenv("GCP_REGION"),
	}

	var err error
	suite.manager, err = testenv.NewCloudEnvManager(ctx, cfg, verbose)
	require.NoError(suite.T(), err)

	suite.T().Log("Creating GKE test environment")
	start := time.Now()
	require.NoError(suite.T(), suite.manager.Create(ctx))
	suite.T().Logf("Total cluster creation time: %s", time.Since(start))

	suite.T().Log("Installing cluster applications")
	start = time.Now()
	require.NoError(suite.T(), suite.manager.HelmfileApply(ctx, "helmfile.yaml"))
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
}

func (suite *GKETestSuite) TearDownSuite() {
	suite.T().Log("Tearing down test cluster")
	require.NoError(suite.T(), suite.manager.Destroy(context.Background()))
}

func (suite *GKETestSuite) TestInvalidImageBuild() {
	ctx := context.Background()
	client := suite.hephClient.HephaestusV1().ImageBuilds(corev1.NamespaceDefault)

	tt := []struct {
		name        string
		errContains string
		mutator     func(build *hephv1.ImageBuild)
	}{
		{
			"blank_context",
			"spec.context: Required value: must not be blank",
			func(build *hephv1.ImageBuild) {
				build.Spec.Context = ""
			},
		},
		{
			"no_images",
			"spec.images: Required value: must contain at least 1 image",
			func(build *hephv1.ImageBuild) {
				build.Spec.Images = nil
			},
		},
		{
			"invalid_image",
			"spec.images: Invalid value: \"~cruisin' usa!!!\": invalid reference format",
			func(build *hephv1.ImageBuild) {
				build.Spec.Images = []string{
					"~cruisin' usa!!!",
				}
			},
		},
		{
			"blank_auth_server",
			"spec.registryAuth[0].server: Required value: must not be blank",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "",
						BasicAuth: &hephv1.BasicAuthCredentials{
							Username: "username",
							Password: "password",
						},
					},
				}
			},
		},
		{
			"no_auth_credential_sources",
			"spec.registryAuth[0]: Required value: must specify 1 credential source",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "docker-registry.default:5000",
					},
				}
			},
		},
		{
			"multiple_auth_credential_sources",
			"spec.registryAuth[0]: Forbidden: cannot specify more than 1 credential source",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "docker-registry.default:5000",
						BasicAuth: &hephv1.BasicAuthCredentials{
							Username: "username",
							Password: "password",
						},
						Secret: &hephv1.SecretCredentials{
							Name:      "name",
							Namespace: "namespace",
						},
					},
				}
			},
		},
		{
			"no_username_basic_auth_credentials",
			"spec.registryAuth[0].basicAuth.username: Required value: must not be blank",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "docker-registry.default:5000",
						BasicAuth: &hephv1.BasicAuthCredentials{
							Password: "password",
						},
					},
				}
			},
		},
		{
			"no_password_basic_auth_credentials",
			"spec.registryAuth[0].basicAuth.password: Required value: must not be blank",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "docker-registry.default:5000",
						BasicAuth: &hephv1.BasicAuthCredentials{
							Username: "username",
						},
					},
				}
			},
		},
		{
			"no_name_secret_credentials",
			"spec.registryAuth[0].secret.name: Required value: must not be blank",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "docker-registry.default:5000",
						Secret: &hephv1.SecretCredentials{
							Namespace: "namespace",
						},
					},
				}
			},
		},
		{
			"no_namespace_secret_credentials",
			"spec.registryAuth[0].secret.namespace: Required value: must not be blank",
			func(build *hephv1.ImageBuild) {
				build.Spec.RegistryAuth = []hephv1.RegistryCredentials{
					{
						Server: "docker-registry.default:5000",
						Secret: &hephv1.SecretCredentials{
							Name: "name",
						},
					},
				}
			},
		},
	}

	for _, tc := range tt {
		suite.T().Run(tc.name, func(t *testing.T) {
			build := validImageBuild()
			tc.mutator(build)

			var statusErr *apierrors.StatusError
			_, err := client.Create(ctx, build, metav1.CreateOptions{})
			require.ErrorAs(t, err, &statusErr)

			errStatus := statusErr.ErrStatus

			assert.Equal(t, metav1.StatusFailure, errStatus.Status)
			assert.Equal(t, metav1.StatusReasonInvalid, errStatus.Reason)
			assert.Contains(t, errStatus.Message, tc.errContains)
		})
	}
}

func (suite *GKETestSuite) TestRegistryPush() {
	suite.T().Run("internal", func(t *testing.T) {
		ctx := context.Background()
		tag := RandomString(8)
		build := validImageBuild()
		build.Spec.LogKey = tag
		build.Spec.Images = []string{
			fmt.Sprintf("docker-registry:5000/test-ns/test-repo:%s", tag),
		}

		buildClient := suite.hephClient.HephaestusV1().ImageBuilds(corev1.NamespaceDefault)

		build, err := buildClient.Create(ctx, build, metav1.CreateOptions{})
		require.NoError(t, err)

		watcher, err := buildClient.Watch(ctx, metav1.SingleObject(build.ObjectMeta))
		require.NoError(t, err)
		defer watcher.Stop()

		ctxTimeout, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		var ib *hephv1.ImageBuild
		for loop := true; loop; {
			select {
			case <-ctxTimeout.Done():
				t.Fatal("Build failed to finish within context deadline")
			case event := <-watcher.ResultChan():
				if event.Type != watch.Modified {
					continue
				}

				ib = event.Object.(*hephv1.ImageBuild)
				require.NotEqual(t, hephv1.PhaseFailed, ib.Status.Phase)

				if ib.Status.Phase == hephv1.PhaseSucceeded {
					loop = false
				}
			}
		}

		// assert image delivery
		svc, err := suite.k8sClient.CoreV1().Services(corev1.NamespaceDefault).Get(
			ctx,
			"docker-registry",
			metav1.GetOptions{},
		)
		require.NoError(t, err)

		registryURL, err := url.Parse(fmt.Sprintf("http://%s:%d", svc.Status.LoadBalancer.Ingress[0].IP, 5000))
		require.NoError(t, err)

		hub, err := registry.New(registryURL.String(), "", "")
		require.NoError(t, err)

		tags, err := hub.Tags("test-ns/test-repo")
		require.NoError(t, err)
		assert.Contains(t, tags, tag)

		// assert message delivery

		// assert log delivery
	})

	suite.T().Run("remote", func(t *testing.T) {
	})

	suite.T().Run("build_failure", func(t *testing.T) {
	})
}
