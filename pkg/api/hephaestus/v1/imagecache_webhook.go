package v1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var imagecachelog = logf.Log.WithName("webhooks").WithName("imagecache")

var _ webhook.Validator = &ImageCache{}

func (in *ImageCache) ValidateCreate() error {
	return in.validateImageCache("create")
}

func (in *ImageCache) ValidateUpdate(runtime.Object) error {
	return in.validateImageCache("update")
}

func (in *ImageCache) ValidateDelete() error {
	return nil // not used, just here for interface compliance
}

func (in *ImageCache) validateImageCache(action string) error {
	log := imagecachelog.WithName("validator").WithName(action).WithValues("imagecache", client.ObjectKeyFromObject(in))
	log.Info("Starting validation")

	var errList field.ErrorList
	fp := field.NewPath("spec")

	if errs := validateImages(log, fp.Child("images"), in.Spec.Images); errs != nil {
		errList = append(errList, errs...)
	}
	if errs := validateRegistryAuth(log, fp.Child("registryAuth"), in.Spec.RegistryAuth); errs != nil {
		errList = append(errList, errs...)
	}

	return invalidIfNotEmpty(ImageCacheKind, in.Name, errList)
}
