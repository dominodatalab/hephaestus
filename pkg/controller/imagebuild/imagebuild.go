package imagebuild

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hephapi "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/component"
)

func Register(mgr ctrl.Manager, cfg config.Controller) error {
	return core.NewReconciler(mgr).
		For(&hephapi.ImageBuild{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Component("build-dispatcher", component.BuildDispatcher()).
		WithContextData("config", cfg.Buildkit).
		WithControllerOptions(controller.Options{
			MaxConcurrentReconciles: cfg.ImageBuildMaxConcurrency,
		}).
		Complete()
}
