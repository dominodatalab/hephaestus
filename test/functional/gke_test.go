//go:build functional && gke

package functional

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/clientset"
	"github.com/dominodatalab/testenv"
	"github.com/heroku/docker-registry-client/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	auth "golang.org/x/oauth2/google"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/pointer"
)

func TestGKEFunctionality(t *testing.T) {
	suite.Run(t, new(GKETestSuite))
}

type GKETestSuite struct {
	suite.Suite

	suiteSetupDone bool
	gcpRegistry    string
	gcpRepository  string

	manager    testenv.Manager
	k8sClient  kubernetes.Interface
	hephClient clientset.Interface
}

func (suite *GKETestSuite) SetupSuite() {
	verbose := os.Getenv("VERBOSE_TESTING") == "true"
	region := os.Getenv("GCP_REGION")
	projectID := os.Getenv("GCP_PROJECT_ID")
	k8sVersion := os.Getenv("KUBERNETES_VERSION")
	managerImageTag := os.Getenv("MANAGER_IMAGE_TAG")

	cfg := testenv.GKEConfig{
		Region:                   region,
		ProjectID:                projectID,
		KubernetesVersion:        k8sVersion,
		KubernetesServiceAccount: "default/hephaestus",
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

	suite.T().Log("Creating GKE test environment")
	start := time.Now()
	require.NoError(suite.T(), suite.manager.Create(ctx))
	suite.T().Logf("Total cluster creation time: %s", time.Since(start))

	repoName, err := suite.manager.OutputVar(ctx, "repository")
	require.NoError(suite.T(), err)
	suite.gcpRegistry = fmt.Sprintf("%s-docker.pkg.dev", region)
	suite.gcpRepository = fmt.Sprintf("%s/%s", projectID, repoName)

	gcpServiceAccount, err := suite.manager.OutputVar(ctx, "service_account")
	require.NoError(suite.T(), err)

	helmfileValues := []string{
		"controller.manager.cloudRegistryAuth.gcp.enabled=true",
		fmt.Sprintf("controller.manager.cloudRegistryAuth.gcp.serviceAccount=%s", gcpServiceAccount),
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

func (suite *GKETestSuite) TearDownSuite() {
	suite.T().Log("Tearing down test cluster")
	require.NoError(suite.T(), suite.manager.Destroy(context.Background()))
}

func (suite *GKETestSuite) TestImageBuildValidation() {
	suite.T().Log("Testing image build validation")

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
			"bad_build_args",
			"spec.buildArgs[0]: Invalid value: \"i have no equals sign\": must use a <key>=<value> format, " +
				"spec.buildArgs[1]: Invalid value: \"   =value\": must use a <key>=<value> format",
			func(build *hephv1.ImageBuild) {
				build.Spec.BuildArgs = []string{
					"i have no equals sign",
					"   =value",
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
		suite.T().Logf("Test case: %s", tc.name)
		suite.T().Run(tc.name, func(t *testing.T) {
			build := &hephv1.ImageBuild{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-build-",
				},
				Spec: hephv1.ImageBuildSpec{
					Context: "https://nowhere.com/docker-build-context.tgz",
					Images: []string{
						"registry/org/repo:tag",
					},
				},
			}
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

func (suite *GKETestSuite) TestImageBuilding() {
	ctx := context.Background()

	suite.T().Run("no_auth", func(t *testing.T) {
		build := newImageBuild(
			python39JupyterBuildContext,
			"docker-registry:5000/test-ns/test-repo",
			nil,
		)
		ib := createBuild(t, ctx, suite.hephClient, build)
		require.NotEqual(t, hephv1.PhaseFailed, ib.Status.Phase)

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
		assert.Contains(t, tags, ib.Spec.LogKey)

		testLogDelivery(t, ctx, suite.k8sClient, ib)
		testMessageDelivery(t, ctx, suite.k8sClient, ib)
	})

	suite.T().Run("bad_auth", func(t *testing.T) {
		build := newImageBuild(
			python39JupyterBuildContext,
			"docker-registry-secure:5000/test-ns/test-repo",
			&hephv1.RegistryCredentials{
				Server: "docker-registry-secure:5000",
				BasicAuth: &hephv1.BasicAuthCredentials{
					Username: "bad",
					Password: "stuff",
				},
			},
		)
		ib := createBuild(t, ctx, suite.hephClient, build)

		assert.Equal(t, ib.Status.Phase, hephv1.PhaseFailed)
		assert.Contains(t, ib.Status.Conditions[0].Message, `"docker-registry-secure:5000" client credentials are invalid`)
	})

	suite.T().Run("basic_auth", func(t *testing.T) {
		build := newImageBuild(
			python39JupyterBuildContext,
			"docker-registry-secure:5000/test-ns/test-repo",
			&hephv1.RegistryCredentials{
				Server: "docker-registry-secure:5000",
				BasicAuth: &hephv1.BasicAuthCredentials{
					Username: "test-user",
					Password: "test-password",
				},
			},
		)
		ib := createBuild(t, ctx, suite.hephClient, build)

		assert.Equalf(t, ib.Status.Phase, hephv1.PhaseSucceeded, "failed build with message %q", ib.Status.Conditions[0].Message)
	})

	suite.T().Run("secret_auth", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-secret-",
			},
			Type: corev1.SecretTypeDockerConfigJson,
			StringData: map[string]string{
				corev1.DockerConfigJsonKey: `{"auths":{"docker-registry-secure:5000":{"username":"test-user","password":"test-password"}}}`,
			},
		}
		secretClient := suite.k8sClient.CoreV1().Secrets(corev1.NamespaceDefault)
		secret, err := secretClient.Create(ctx, secret, metav1.CreateOptions{})
		require.NoError(t, err, "failed to create docker credentials secret")
		defer secretClient.Delete(ctx, secret.Name, metav1.DeleteOptions{})

		build := newImageBuild(
			python39JupyterBuildContext,
			"docker-registry-secure:5000/test-ns/test-repo",
			&hephv1.RegistryCredentials{
				Server: "docker-registry-secure:5000",
				Secret: &hephv1.SecretCredentials{
					Name:      secret.Name,
					Namespace: secret.Namespace,
				},
			},
		)
		ib := createBuild(t, ctx, suite.hephClient, build)

		assert.Equalf(t, ib.Status.Phase, hephv1.PhaseSucceeded, "failed build with message %q", ib.Status.Conditions[0].Message)
	})

	suite.T().Run("cloud_auth", func(t *testing.T) {
		image := fmt.Sprintf("%s/test-image", suite.gcpRepository)
		build := newImageBuild(
			python39JupyterBuildContext,
			fmt.Sprintf("%s/%s", suite.gcpRegistry, image),
			&hephv1.RegistryCredentials{
				Server:        suite.gcpRegistry,
				CloudProvided: pointer.Bool(true),
			},
		)
		ib := createBuild(t, ctx, suite.hephClient, build)

		credentials, err := auth.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
		require.NoError(t, err)

		token, err := credentials.TokenSource.Token()
		require.NoError(t, err)

		hub, err := registry.New(fmt.Sprintf("https://%s", suite.gcpRegistry), "oauth2accesstoken", token.AccessToken)
		require.NoError(t, err)

		tags, err := hub.Tags(image)
		require.NoError(t, err)
		assert.Contains(t, tags, ib.Spec.LogKey)
	})

	suite.T().Run("build_args", func(t *testing.T) {
		build := newImageBuild(
			buildArgBuildContext,
			"docker-registry:5000/test-ns/test-repo",
			nil,
		)
		build.Spec.BuildArgs = []string{"INPUT=VAR=VAL"}
		ib := createBuild(t, ctx, suite.hephClient, build)

		assert.Equalf(t, ib.Status.Phase, hephv1.PhaseSucceeded, "failed build with message %q", ib.Status.Conditions[0].Message)
	})

	suite.T().Run("build_failure", func(t *testing.T) {
		build := newImageBuild(
			errorBuildContext,
			"docker-registry:5000/test-ns/test-repo",
			nil,
		)
		ib := createBuild(t, ctx, suite.hephClient, build)

		assert.Equalf(t, ib.Status.Phase, hephv1.PhaseFailed, "expected build with bad Dockerfile to fail")
	})

	suite.T().Run("multi_stage", func(t *testing.T) {
		build := newImageBuild(
			multiStageBuildContext,
			"docker-registry:5000/test-ns/test-repo",
			nil,
		)
		ib := createBuild(t, ctx, suite.hephClient, build)

		assert.Equalf(t, ib.Status.Phase, hephv1.PhaseSucceeded, "failed build with message %q", ib.Status.Conditions[0].Message)
	})

	suite.T().Run("concurrent_builds", func(t *testing.T) {
		var wg sync.WaitGroup
		ch := make(chan *hephv1.ImageBuild, 3)

		for i := 0; i < 3; i++ {
			wg.Add(1)

			go func() {
				defer wg.Done()

				build := newImageBuild(
					dseBuildContext,
					"docker-registry:5000/test-ns/test-repo",
					nil,
				)
				build.Spec.DisableLocalBuildCache = true

				ch <- createBuild(t, ctx, suite.hephClient, build)
			}()
		}
		wg.Wait()
		close(ch)

		var builders []string
		for ib := range ch {
			builders = append(builders, ib.Status.BuilderAddr)
			assert.Equalf(
				t,
				ib.Status.Phase,
				hephv1.PhaseSucceeded,
				"failed build %q with message %q",
				ib.Name,
				ib.Status.Conditions[0].Message,
			)
		}

		expected := []string{
			"tcp://hephaestus-buildkit-0.hephaestus-buildkit.default:1234",
			"tcp://hephaestus-buildkit-1.hephaestus-buildkit.default:1234",
			"tcp://hephaestus-buildkit-2.hephaestus-buildkit.default:1234",
		}
		assert.ElementsMatch(t, builders, expected, "builds did not execute on unique buildkit pods")
	})
}
