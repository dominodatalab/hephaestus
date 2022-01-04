package metadata

import (
	"fmt"

	"github.com/dominodatalab/controller-util/metadata"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

var meta = metadata.NewProvider(
	"image-build",
	metadata.WithCreator("hephaestus-controller"),
)

func AssertImageBuild(obj client.Object) *hephv1.ImageBuild {
	ib, ok := obj.(*hephv1.ImageBuild)
	if !ok {
		panic(fmt.Errorf("type assertion failed: %v is not an ImageBuild", obj))
	}

	return ib
}

func ResourceLabels(obj client.Object) map[string]string {
	return meta.StandardLabels(obj, metadata.AppComponentNone, nil)
}

func ResourceNamespace(obj client.Object) string {
	return obj.GetNamespace()
}

func ConfigMapName(obj client.Object) string {
	return commonInstanceName(obj)
}

func JobName(obj client.Object) string {
	return commonInstanceName(obj)
}

func RoleName(obj client.Object) string {
	return commonInstanceName(obj)
}

func ServiceAccountName(obj client.Object) string {
	return commonInstanceName(obj)
}

func commonInstanceName(obj client.Object) string {
	return meta.InstanceName(obj, metadata.AppComponentNone)
}
