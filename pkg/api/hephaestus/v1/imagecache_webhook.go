package v1

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var imagecachelog = logf.Log.WithName("webhook").WithName("imagecache")

var _ admission.Defaulter[*ImageCache] = &ImageCache{}

func (in *ImageCache) Default(_ context.Context, obj *ImageCache) error {
	log := imagecachelog.WithName("defaulter").WithValues("imagecache", client.ObjectKeyFromObject(obj))
	log.V(1).Info("Applying default values")

	// admission.WithDefaulter binds Default's receiver (in) to a single static template registered at
	// startup, not the decoded request object - obj is the one whose mutations the webhook response
	// actually applies, so defaults must be written onto obj, not in.
	obj.Spec.Platforms = normalizePlatforms(obj.Spec.Platforms)

	return nil
}

var _ admission.Validator[*ImageCache] = &ImageCache{}

func (in *ImageCache) ValidateCreate(_ context.Context, obj *ImageCache) (admission.Warnings, error) {
	return obj.validateImageCache("create")
}

func (in *ImageCache) ValidateUpdate(_ context.Context, _ *ImageCache, newObj *ImageCache) (admission.Warnings, error) {
	return newObj.validateImageCache("update")
}

func (in *ImageCache) ValidateDelete(context.Context, *ImageCache) (admission.Warnings, error) {
	return admission.Warnings{}, nil
}

// validateImageCache validates in (the actual per-request object - see ValidateCreate/ValidateUpdate,
// which call this as obj.validateImageCache(...) so obj becomes the receiver here), not the empty
// static template admission.WithValidator registers in as its Validator[*ImageCache] at startup.
func (in *ImageCache) validateImageCache(action string) (admission.Warnings, error) {
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
	if errs := validatePlatforms(log, fp.Child("platforms"), in.Spec.Platforms, platformCapabilities); errs != nil {
		errList = append(errList, errs...)
	}

	return admission.Warnings{}, invalidIfNotEmpty(ImageCacheKind, in.Name, errList)
}
