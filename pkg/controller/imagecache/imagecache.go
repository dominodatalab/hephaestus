package imagecache

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache/component"
)

func Register(mgr ctrl.Manager, cfg config.Controller) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageCache{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Component("cache-warmer", component.CacheWarmer(cfg.Buildkit)).
		WithWebhooks().
		Complete()
}
