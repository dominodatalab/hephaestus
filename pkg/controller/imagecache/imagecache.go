package imagecache

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagecache/component"
)

func Register(mgr ctrl.Manager, config config.BuildkitConfig) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageCache{}).
		WithContextData("config", config).
		Component("cache-warmer", component.CacheWarmer()).
		Component("pod-monitor", component.PodMonitor()).
		Complete()
}
