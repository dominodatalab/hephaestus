package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache"
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
	log.Info("Using provided configuration", "config", cfg)

	log.Info("Adding API types to runtime scheme")
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = hephv1.AddToScheme(scheme)

	// +kubebuilder:scaffold:scheme

	opts := ctrl.Options{
		Scheme:                 scheme,
		Port:                   cfg.Manager.WebhookPort,
		MetricsBindAddress:     cfg.Manager.MetricsAddr,
		HealthProbeBindAddress: cfg.Manager.HealthProbeAddr,
		LeaderElection:         cfg.Manager.EnableLeaderElection,
		LeaderElectionID:       "hephaestus-controller-lock",
	}

	if len(cfg.Manager.WatchNamespaces) > 0 {
		log.Info("Limiting reconciliation watch", "namespaces", cfg.Manager.WatchNamespaces)
		opts.NewCache = cache.MultiNamespacedCacheBuilder(cfg.Manager.WatchNamespaces)
	} else {
		log.Info("Watching all namespaces")
	}

	log.Info("Creating new controller manager")
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		return err
	}
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	log.Info("Registering ImageBuild controller")
	if err = imagebuild.Register(mgr, cfg); err != nil {
		return err
	}

	log.Info("Registering ImageCache controller")
	if err = imagecache.Register(mgr, cfg); err != nil {
		return err
	}

	// +kubebuilder:scaffold:builder

	log.Info("Starting controller manager")
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return err
	}

	return nil
}
