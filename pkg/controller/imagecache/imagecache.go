package imagecache

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

func Register(_ ctrl.Manager, _ config.Controller) error {
	ctrl.Log.WithName("controller").WithName("imagecache").Info(
		"Aborting registration, requires rework after other changes",
	)
	return nil

	// return core.NewReconciler(mgr).
	// 	For(&hephv1.ImageCache{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
	// 	Component("cache-warmer", component.CacheWarmer(cfg.Buildkit)).
	// 	WithWebhooks().
	// 	Complete()
}
