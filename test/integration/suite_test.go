//go:build integration

// Package integration contains lightweight functional tests that exercise the
// public controller entrypoint against a real (envtest) API server, without the
// heavy cloud-cluster machinery of test/functional.
//
// This is more complex than the way kubebuilder does it, but seeing as we
// Regressed on how we start the manager, this is a more complete integration.
package integration

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller"
)

var (
	ctx       context.Context
	testEnv   *envtest.Environment
	k8sClient client.Client
)

// TestMain bootstraps a single envtest API server and a running controller
// manager for the whole package, then tears them down.
func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		log.Fatalf("integration suite setup failed: %v", err)
	}
	os.Exit(code)
}

func runSuite(m *testing.M) (int, error) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	if err := hephv1.AddToScheme(scheme.Scheme); err != nil {
		return 0, fmt.Errorf("add scheme: %w", err)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "deployments", "crds")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("testdata")},
		},
	}
	if err := resolveEnvtestAssets(testEnv); err != nil {
		return 0, err
	}
	cfg, err := testEnv.Start()
	if err != nil {
		return 0, fmt.Errorf("start envtest: %w", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			log.Printf("stop envtest: %v", err)
		}
	}()

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return 0, fmt.Errorf("build client: %w", err)
	}

	if err := startController(cfg, testEnv.WebhookInstallOptions); err != nil {
		return 0, fmt.Errorf("start controller: %w", err)
	}

	code := m.Run()

	// controller.Start runs the manager on ctrl.SetupSignalHandler()'s context,
	// which we can't cancel directly. Signal ourselves so the manager shuts down
	// and releases the API server before envtest stops it; otherwise envtest's
	// Stop() blocks on its kube-apiserver stop timeout.
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	time.Sleep(2 * time.Second)

	return code, nil
}

func resolveEnvtestAssets(env *envtest.Environment) error {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return nil
	}
	args := []string{"use", "-p", "path"}
	if v := os.Getenv("ENVTEST_K8S_VERSION"); v != "" {
		args = []string{"use", v, "-p", "path"}
	}
	out, err := exec.Command("setup-envtest", args...).Output()
	if err != nil {
		return fmt.Errorf("locate envtest binaries with setup-envtest (run `make tools` to install it): %w", err)
	}
	env.BinaryAssetsDirectory = strings.TrimSpace(string(out))
	return nil
}

// startController launches the public controller.Start in the background, wired
// to the envtest API server (via KUBECONFIG) and the webhook serving cert/port
// that envtest provisioned.
func startController(restCfg *rest.Config, whOpts envtest.WebhookInstallOptions) error {
	kubeconfig := filepath.Join(os.TempDir(), "hephaestus-integration-kubeconfig")
	if err := writeKubeconfig(kubeconfig, restCfg); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"KUBECONFIG":                      kubeconfig,
		"WEBHOOK_SERVER_CERT_DIR":         whOpts.LocalServingCertDir,
		"CLOUD_AUTH_REGISTRATION_TIMEOUT": "2s",
	} {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}

	cfg := config.Controller{
		Logging: config.Logging{
			StacktraceLevel: "error",
			Container:       config.ContainerLogging{Encoder: "console", LogLevel: "info"},
			Logfile:         config.LogfileLogging{Enabled: false},
		},
		Manager: config.Manager{
			HealthProbeAddr: "0",
			MetricsAddr:     "0",
			WebhookPort:     whOpts.LocalServingPort,
			WatchNamespaces: []string{"default"},
			ImageBuild:      config.ImageBuild{Concurrency: 1, HistoryLimit: 10, Interval: 1},
		},
		Buildkit: config.Buildkit{
			Namespace:       "default",
			DaemonPort:      1234,
			ServiceName:     "hephaestus-buildkit",
			StatefulSetName: "hephaestus-buildkit",
			PodLabels:       map[string]string{"app.kubernetes.io/name": "hephaestus"},
		},
		Messaging: config.Messaging{Enabled: false},
		NewRelic:  config.NewRelic{Enabled: false},
	}

	go func() { _ = controller.Start(cfg) }()
	return nil
}

// writeKubeconfig serializes a rest.Config to a kubeconfig file so that
// controller.Start can pick it up via ctrl.GetConfigOrDie.
func writeKubeconfig(path string, rc *rest.Config) error {
	c := clientcmdapi.NewConfig()
	c.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   rc.Host,
		CertificateAuthorityData: rc.CAData,
	}
	c.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: rc.CertData,
		ClientKeyData:         rc.KeyData,
	}
	c.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	c.CurrentContext = "envtest"
	return clientcmd.WriteToFile(*c, path)
}
