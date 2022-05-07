package containerimagebuild

import (
	"context"

	"github.com/dominodatalab/controller-util/core"
	"github.com/dominodatalab/hephaestus/pkg/crd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	forgev1alpha1 "github.com/dominodatalab/hephaestus/pkg/api/forge/v1alpha1"
	"github.com/dominodatalab/hephaestus/pkg/controller/containerimagebuild/component"
)

// Register the container image build controller.
//
// This controller and its corresponding component.ConversionShimComponent convert
// forge v1alpha1.ContainerImageBuild resources into hephaestus v1.ImageBuild resources
// with the intent of incrementally migrating away from the Forge API.
//
// This controller should be removed once forge is fully obsolete.
func Register(mgr ctrl.Manager) error {
	cibExists, err := crd.Exists(context.Background(), metav1.GroupVersion{
		Group: forgev1alpha1.SchemeGroupVersion.Group, Version: forgev1alpha1.SchemeGroupVersion.Version,
	})
	if !cibExists || err != nil {
		return err
	}

	return core.NewReconciler(mgr).
		For(&forgev1alpha1.ContainerImageBuild{}).
		Component("conversion-shim", component.ConversionShim()).
		Complete()
}
