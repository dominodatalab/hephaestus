package controller

import (
	"context"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	forgev1alpha1 "github.com/dominodatalab/hephaestus/pkg/api/forge/v1alpha1"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/workerpool"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/containerimagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/logger"
	// +kubebuilder:scaffold:imports
)

// Start creates a new controller manager, registers controllers, and starts
// their control loops for resource reconciliation.
func Start(cfg config.Controller) error {
	l, err := logger.New(cfg.Logging)
	if err != nil {
		return err
	}
	ctrl.SetLogger(l)

	log := ctrl.Log.WithName("setup")
	log.V(1).Info("Using provided configuration", "config", cfg)

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

	if err = registerControllers(log, mgr, pool, cfg); err != nil {
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

func createWorkerPool(ctx context.Context, log logr.Logger, mgr ctrl.Manager, cfg config.Buildkit) (workerpool.Pool, error) {
	log.Info("Initializing buildkit worker pool")
	poolOpts := []workerpool.PoolOption{
		workerpool.Logger(ctrl.Log.WithName("buildkit.workerpool")),
	}

	if mit := cfg.PoolMaxIdleTime; mit != nil {
		poolOpts = append(poolOpts, workerpool.MaxIdleTime(*mit))
	}

	if swt := cfg.PoolSyncWaitTime; swt != nil {
		poolOpts = append(poolOpts, workerpool.SyncWaitTime(*swt))
	}

	pool, err := workerpool.New(ctx, mgr.GetConfig(), cfg, poolOpts...)
	if err != nil {
		return nil, err
	}

	return pool, nil
}

func registerControllers(log logr.Logger, mgr ctrl.Manager, pool workerpool.Pool, cfg config.Controller) error {
	log.Info("Registering ContainerImageBuild controller")
	if err := containerimagebuild.Register(mgr); err != nil {
		return err
	}

	log.Info("Registering ImageBuild controller")
	if err := imagebuild.Register(mgr, cfg, pool); err != nil {
		return err
	}

	log.Info("Registering ImageBuildStatus controller")
	if err := imagebuild.RegisterImageBuildStatus(mgr, cfg); err != nil {
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
