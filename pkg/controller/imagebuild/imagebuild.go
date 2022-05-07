package imagebuild

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/workerpool"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/component"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/predicate"
)

func Register(mgr ctrl.Manager, cfg config.Controller, pool workerpool.Pool) error {
	bd, err := component.BuildDispatcher(cfg.Buildkit, pool)
	if err != nil {
		return err
	}
	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}).
		Component("build-dispatcher", bd).
		WithControllerOptions(controller.Options{MaxConcurrentReconciles: cfg.ImageBuildMaxConcurrency}).
		WithWebhooks().
		Complete()
}

func RegisterImageBuildStatus(mgr ctrl.Manager, cfg config.Controller) error {
	return core.NewReconciler(mgr).
		Named("imagebuildstatus").
		For(&hephv1.ImageBuild{}, builder.WithPredicates(predicate.UnprocessedTransitionsPredicate{})).
		Component("status-messenger", component.StatusMessenger(cfg.Messaging)).
		Complete()
}
