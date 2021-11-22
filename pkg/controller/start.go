package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache"
	// +kubebuilder:scaffold:imports
)

func Start(cfg config.Config) error {
	zapOpts, err := cfg.Logging.ZapOptions()
	if err != nil {
		return err
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))

	log := ctrl.Log.WithName("setup")

	log.Info("Adding API types to runtime scheme")
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = hephv1.AddToScheme(scheme)

	// +kubebuilder:scaffold:scheme

	opts := ctrl.Options{
		Scheme:                 scheme,
		Port:                   cfg.Manager.WebhookPort,
		MetricsBindAddress:     cfg.Manager.MetricsAddr,
		HealthProbeBindAddress: cfg.Manager.ProbeAddr,
		LeaderElection:         cfg.Manager.EnableLeaderElection,
		LeaderElectionID:       "hephaestus-controller-lock",
	}

	log.Info("Creating new controller manager")
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		return err
	}

	log.Info("Registering ImageBuild controller")
	if err = imagebuild.Register(mgr, cfg.Buildkit); err != nil {
		return err
	}

	log.Info("Registering ImageCache controller")
	if err = imagecache.Register(mgr, cfg.Buildkit); err != nil {
		return err
	}

	// +kubebuilder:scaffold:builder

	log.Info("Starting controller manager")
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return err
	}

	return nil
}
