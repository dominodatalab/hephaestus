package containerimagebuild

import (
	"github.com/dominodatalab/controller-util/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	forgev1alpha1 "github.com/dominodatalab/hephaestus/pkg/api/forge/v1alpha1"
	"github.com/dominodatalab/hephaestus/pkg/controller/containerimagebuild/component"
	"github.com/dominodatalab/hephaestus/pkg/crd"
)

// Register the container image build controller.
//
// This controller and its corresponding component.ConversionShimComponent convert
// forge v1alpha1.ContainerImageBuild resources into hephaestus v1.ImageBuild resources
// with the intent of incrementally migrating away from the Forge API.
//
// This controller should be removed once forge is fully obsolete.
func Register(mgr ctrl.Manager) error {
	forgev1alpha1Exists, err := crd.Exists(metav1.GroupVersion{
		Group: forgev1alpha1.SchemeGroupVersion.Group, Version: forgev1alpha1.SchemeGroupVersion.Version,
	})
	if err != nil {
		return err
	}

	if !forgev1alpha1Exists {
		ctrl.Log.WithName("controller").WithName("containerimagebuild").Info(
			"Not registering ContainerImageBuild controller, API group does not exist",
			"groupVersion", forgev1alpha1.SchemeGroupVersion.String(),
		)
		return nil
	}

	return core.NewReconciler(mgr).
		For(&forgev1alpha1.ContainerImageBuild{}).
		Component("conversion-shim", component.ConversionShim()).
		Complete()
}
