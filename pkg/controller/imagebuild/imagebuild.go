package imagebuild

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/component"
)

func Register(mgr ctrl.Manager, config config.BuildkitConfig) error {
	// NOTE: this will eventually launch a build job

	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}).
		WithContextData("config", config).
		Component("in-process-build", component.InProcessBuild()).
		Complete()
}
