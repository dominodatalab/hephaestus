package v1

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var imagecachelog = logf.Log.WithName("webhook").WithName("imagecache")

var _ admission.Validator[*ImageCache] = &ImageCache{}

func (in *ImageCache) ValidateCreate(_ context.Context, obj *ImageCache) (admission.Warnings, error) {
	return in.validateImageCache("create", obj)
}

func (in *ImageCache) ValidateUpdate(_ context.Context, _ *ImageCache, newObj *ImageCache) (admission.Warnings, error) {
	return in.validateImageCache("update", newObj)
}

func (in *ImageCache) ValidateDelete(_ context.Context, _ *ImageCache) (admission.Warnings, error) {
	return admission.Warnings{}, nil
}

func (in *ImageCache) validateImageCache(action string, obj *ImageCache) (admission.Warnings, error) {
	log := imagecachelog.WithName("validator").WithName(action).WithValues("imagecache", client.ObjectKeyFromObject(obj))
	log.Info("Starting validation")

	var errList field.ErrorList
	fp := field.NewPath("spec")

	if errs := validateImages(log, fp.Child("images"), obj.Spec.Images); errs != nil {
		errList = append(errList, errs...)
	}
	if errs := validateRegistryAuth(log, fp.Child("registryAuth"), obj.Spec.RegistryAuth); errs != nil {
		errList = append(errList, errs...)
	}

	return admission.Warnings{}, invalidIfNotEmpty(ImageCacheKind, obj.Name, errList)
}
