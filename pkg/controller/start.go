package controller

import (
	"context"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/newrelic/go-agent/v3/integrations/nrzap"
	"github.com/newrelic/go-agent/v3/newrelic"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	forgev1alpha1 "github.com/dominodatalab/hephaestus/pkg/api/forge/v1alpha1"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/worker"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/containerimagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/kubernetes"
	"github.com/dominodatalab/hephaestus/pkg/logger"
	// +kubebuilder:scaffold:imports
)

// Start creates a new controller manager, registers controllers, and starts
// their control loops for resource reconciliation.
func Start(cfg config.Controller) error {
	zapLogger, err := logger.NewZap(cfg.Logging)
	if err != nil {
		return err
	}

	ctrl.SetLogger(zapr.NewLogger(zapLogger))

	log := ctrl.Log.WithName("setup")
	log.V(1).Info("Using provided configuration", "config", cfg)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := createWorkerPool(ctx, log, mgr, cfg.Buildkit)
	if err != nil {
		return err
	}

	if err = registerControllers(log, mgr, pool, nr, cfg); err != nil {
		return err
	}

	log.Info("Registering cloud auth providers")
	if err = credentials.LoadCloudProviders(log); err != nil {
		return err
	}

	// +kubebuilder:scaffold:builder

	log.Info("Starting controller manager")
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return err
	}

	return nil
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
	_ = clientgoscheme.AddToScheme(scheme)
	_ = hephv1.AddToScheme(scheme)
	_ = forgev1alpha1.AddToScheme(scheme)

	// +kubebuilder:scaffold:scheme

	opts := ctrl.Options{
		Scheme:                 scheme,
		Port:                   cfg.WebhookPort,
		MetricsBindAddress:     cfg.MetricsAddr,
		HealthProbeBindAddress: cfg.HealthProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "hephaestus-controller-lock",
	}

	if certDir := os.Getenv("WEBHOOK_SERVER_CERT_DIR"); certDir != "" {
		log.Info("Overriding webhook server certificate directory", "value", certDir)
		opts.WebhookServer = &webhook.Server{
			CertDir: certDir,
		}
	}

	if len(cfg.WatchNamespaces) > 0 {
		log.Info("Limiting reconciliation watch", "namespaces", cfg.WatchNamespaces)
		opts.NewCache = cache.MultiNamespacedCacheBuilder(cfg.WatchNamespaces)
	} else {
		log.Info("Watching all namespaces")
	}

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

func createWorkerPool(ctx context.Context, log logr.Logger, mgr ctrl.Manager, cfg config.Buildkit) (worker.Pool, error) {
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

	if wt := cfg.PoolWatchTimeout; wt != nil {
		poolOpts = append(poolOpts, worker.WatchTimeoutSeconds(*wt))
	}

	clientset, err := kubernetes.Clientset(mgr.GetConfig())
	if err != nil {
		return nil, err
	}

	return worker.NewPool(ctx, clientset, cfg, poolOpts...), nil
}

func registerControllers(log logr.Logger, mgr ctrl.Manager, pool worker.Pool, nr *newrelic.Application, cfg config.Controller) error {
	log.Info("Registering ContainerImageBuild controller")
	if err := containerimagebuild.Register(mgr); err != nil {
		return err
	}

	log.Info("Registering ImageBuild controller")
	if err := imagebuild.Register(mgr, cfg, pool, nr); err != nil {
		return err
	}

	log.Info("Registering ImageBuildStatus controller")
	if err := imagebuild.RegisterImageBuildStatus(mgr, cfg, nr); err != nil {
		return err
	}

	log.Info("Registering ImageBuildDelete controller")
	if err := imagebuild.RegisterImageBuildDelete(mgr); err != nil {
		return err
	}

	log.Info("Registering ImageCache controller")
	if err := imagecache.Register(mgr, cfg); err != nil {
		return err
	}

	return nil
}
