package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/newrelic/go-agent/v3/integrations/nrzap"
	"github.com/newrelic/go-agent/v3/newrelic"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/worker"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuildmessage"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/kubernetes"
	"github.com/dominodatalab/hephaestus/pkg/logger"
	// +kubebuilder:scaffold:imports
)

var cloudAuthRegistrationTimeout = 30 * time.Second

func init() {
	if cloudAuthRegTimeoutEnv := os.Getenv("CLOUD_AUTH_REGISTRATION_TIMEOUT"); cloudAuthRegTimeoutEnv != "" {
		if duration, err := time.ParseDuration(cloudAuthRegTimeoutEnv); err == nil {
			cloudAuthRegistrationTimeout = duration
		}
	}
}

// Start creates a new controller manager, registers controllers, and starts
// their control loops for resource reconciliation.
func Start(cfg config.Controller) error {
	zapLogger, err := logger.NewZap(cfg.Logging)
	if err != nil {
		return err
	}

	ctrl.SetLogger(zapr.NewLogger(zapLogger))

	log := ctrl.Log.WithName("setup")
	log.Info("Using provided configuration", "config", cfg)

	log.Info("Configuring NewRelic application")
	nr, err := configureNewRelic(zapLogger, cfg.NewRelic)
	if err != nil {
		return err
	}
	defer nr.Shutdown(5 * time.Second)

	mgr, err := createManager(log, cfg.Manager)
	if err != nil {
		return err
	}

	pool, err := createWorkerPool(log, mgr, cfg.Buildkit)
	if err != nil {
		return err
	}
	if err = mgr.Add(pool); err != nil {
		return err
	}

	if err = registerControllers(log, mgr, pool, nr, cfg); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cloudAuthRegistrationTimeout)
	defer cancel()

	log.Info("Registering cloud auth providers", "timeout", cloudAuthRegistrationTimeout)
	if err = credentials.LoadCloudProviders(ctx, log); err != nil {
		return err
	}

	// +kubebuilder:scaffold:builder

	log.Info("Starting controller manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}

func configureNewRelic(log *zap.Logger, cfg config.NewRelic) (*newrelic.Application, error) {
	return newrelic.NewApplication(
		newrelic.ConfigEnabled(cfg.Enabled),
		newrelic.ConfigAppName(cfg.AppName),
		newrelic.ConfigLicense(cfg.LicenseKey),
		nrzap.ConfigLogger(log),
		func(c *newrelic.Config) {
			c.Labels = cfg.Labels
		},
	)
}

func createManager(log logr.Logger, cfg config.Manager) (ctrl.Manager, error) {
	log.Info("Adding API types to runtime scheme")
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(hephv1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme

	opts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.HealthProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "hephaestus-controller-lock",
	}
	webhookOpts := webhook.Options{Port: cfg.WebhookPort}

	if certDir := os.Getenv("WEBHOOK_SERVER_CERT_DIR"); certDir != "" {
		log.Info("Overriding webhook server certificate directory", "value", certDir)
		webhookOpts.CertDir = certDir
	}

	if len(cfg.WatchNamespaces) > 0 {
		log.Info("Limiting reconciliation watch", "namespaces", cfg.WatchNamespaces)
		defaultNS := map[string]cache.Config{}
		for _, ns := range cfg.WatchNamespaces {
			defaultNS[ns] = cache.Config{}
		}
		opts.Cache = cache.Options{DefaultNamespaces: defaultNS}
	} else {
		log.Info("Watching all namespaces")
	}
	opts.WebhookServer = webhook.NewServer(webhookOpts)
	opts.WebhookServer.Register("/debugz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		log.Info("Debug endpoint called")

		// Get the webhook mux
		mux := opts.WebhookServer.WebhookMux()
		if mux == nil {
			_, err := w.Write([]byte("Webhook server is running, but mux is nil"))
			if err != nil {
				log.Error(err, "Failed to write response.")
				return
			}
			return
		}

		// We can use a set of common paths to test
		w.Header().Set("Content-Type", "text/plain")
		_, err := w.Write([]byte("Webhook server is running\n\nRegistered paths:\n"))
		if err != nil {
			log.Error(err, "Failed to write response.")
			return
		}

		// Common webhook paths to test
		testPaths := []string{
			"/",
			"/mutate-hephaestus-dominodatalab-com-v1-imagebuild",
			"/validate-hephaestus-dominodatalab-com-v1-imagebuild",
			"/mutate-imagebuild.hephaestus.dominodatalab.com",
			"/validate-imagebuild.hephaestus.dominodatalab.com",
			"/convert",
			"/healthz",
			"/readyz",
		}

		gvks := []struct{ group, version, kind string }{
			{"hephaestus.dominodatalab.com", "v1", "imagebuild"},
			{"hephaestus.dominodatalab.com", "v1", "imagecache"},
		}

		for _, gvk := range gvks {
			testPaths = append(testPaths,
				fmt.Sprintf("/mutate-%s-%s-%s", strings.ReplaceAll(gvk.group, ".", "-"), gvk.version, gvk.kind),
				fmt.Sprintf("/validate-%s-%s-%s", strings.ReplaceAll(gvk.group, ".", "-"), gvk.version, gvk.kind),
			)
		}

		// Test each path
		for _, path := range testPaths {
			handler, pattern := mux.Handler(&http.Request{URL: &url.URL{Path: path}})
			status := "✓ Registered"
			if pattern != path || handler == nil {
				status = "✗ Not registered"
			}
			_, err := w.Write([]byte(fmt.Sprintf("%s: %s\n", path, status)))
			if err != nil {
				log.Error(err, "Failed to write response for path", "path", path)
				return
			}
		}
	}))

	opts.WebhookServer.Register("/livez", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		log.Info("Webhook livez endpoint called")

		// Get the webhook mux
		mux := opts.WebhookServer.WebhookMux()
		if mux == nil {
			_, err := w.Write([]byte("Webhook server is running, but mux is nil"))
			if err != nil {
				log.Error(err, "Failed to write response.")
				return
			}
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		_, err := w.Write([]byte("Webhook server is live."))
		if err != nil {
			log.Error(err, "Failed to write response")
			return
		}
	}))

	log.Info("Creating new controller manager")
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		return nil, err
	}
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, err
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, err
	}

	return mgr, nil
}

func createWorkerPool(
	log logr.Logger,
	mgr ctrl.Manager,
	cfg config.Buildkit,
) (worker.Pool, error) {
	log.Info("Initializing buildkit worker pool")
	poolOpts := []worker.PoolOption{
		worker.Logger(ctrl.Log.WithName("buildkit.worker-pool")),
	}

	if mit := cfg.PoolMaxIdleTime; mit != nil {
		poolOpts = append(poolOpts, worker.MaxIdleTime(*mit))
	}

	if swt := cfg.PoolSyncWaitTime; swt != nil {
		poolOpts = append(poolOpts, worker.SyncWaitTime(*swt))
	}

	if wt := cfg.PoolEndpointWatchTimeout; wt != nil {
		poolOpts = append(poolOpts, worker.EndpointWatchTimeoutSeconds(*wt))
	}

	clientset, err := kubernetes.Clientset(mgr.GetConfig())
	if err != nil {
		return nil, err
	}

	return worker.NewPool(clientset, cfg, poolOpts...), nil
}

func registerControllers(
	log logr.Logger,
	mgr ctrl.Manager,
	pool worker.Pool,
	nr *newrelic.Application,
	cfg config.Controller,
) error {
	deleteCh := make(chan client.ObjectKey, 10)

	log.Info("Registering ImageBuild controller")
	if err := imagebuild.Register(mgr, cfg, pool, nr, deleteCh); err != nil {
		return err
	}

	log.Info("Registering ImageBuildMessage controller")
	if err := imagebuildmessage.Register(mgr, cfg, nr); err != nil {
		return err
	}

	log.Info("Registering ImageBuildDelete controller")
	if err := imagebuild.RegisterImageBuildDelete(mgr, deleteCh); err != nil {
		return err
	}

	log.Info("Registering ImageCache controller")
	return imagecache.Register(mgr, cfg)
}
