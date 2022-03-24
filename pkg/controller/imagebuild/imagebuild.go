package imagebuild

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/workerpool"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/component"
)

func Register(mgr ctrl.Manager, cfg config.Controller, pool workerpool.Pool) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}).
		Component("build-dispatcher", component.BuildDispatcher(cfg.Buildkit, pool)).
		Component("status-messenger", component.StatusMessenger(cfg.Messaging)).
		WithControllerOptions(controller.Options{MaxConcurrentReconciles: cfg.ImageBuildMaxConcurrency}).
		WithWebhooks().
		Complete()
}
