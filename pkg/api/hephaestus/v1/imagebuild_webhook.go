package v1

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/docker/distribution/reference"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const defaultCacheTag = "general"

var imagebuildlog = logf.Log.WithName("webhooks").WithName("imagebuild")

var _ webhook.Defaulter = &ImageBuild{}

func (in *ImageBuild) Default() {
	log := imagebuildlog.WithName("defaulter").WithValues("imagebuild", client.ObjectKeyFromObject(in))
	log.Info("Applying default values")

	spec := &in.Spec
	if spec.CacheMode == "" {
		log.Info("Setting default cache mode", "value", CacheModeMin)
		spec.CacheMode = CacheModeMin
	}
	if spec.CacheTag == "" {
		log.Info("Setting default cache tag", "value", defaultCacheTag)
		spec.CacheTag = defaultCacheTag
	}
}

var _ webhook.Validator = &ImageBuild{}

func (in *ImageBuild) ValidateCreate() error {
	return in.validateImageBuild("create")
}

func (in *ImageBuild) ValidateUpdate(runtime.Object) error {
	return in.validateImageBuild("update")
}

func (in *ImageBuild) ValidateDelete() error {
	return nil // not used, just here for interface compliance
}

func (in *ImageBuild) validateImageBuild(action string) error {
	log := imagebuildlog.WithName("validator").WithName(action).WithValues("imagebuild", client.ObjectKeyFromObject(in))
	log.Info("Starting validation")

	var errList field.ErrorList
	fp := field.NewPath("spec")

	if strings.TrimSpace(in.Spec.Context) == "" {
		log.V(1).Info("Context is blank")
		errList = append(errList, field.Required(fp.Child("context"), "must not be blank"))
	} else if _, err := url.ParseRequestURI(in.Spec.Context); err != nil {
		log.V(1).Info("Context is not a valid URL")
		errList = append(errList, field.Invalid(fp.Child("context"), in.Spec.Context, err.Error()))
	}

	if errs := validateImages(log, fp.Child("images"), in.Spec.Images); errs != nil {
		errList = append(errList, errs...)
	}

	for idx, arg := range in.Spec.BuildArgs {
		if len(strings.Split(arg, "=")) != 2 {
			log.V(1).Info("Build arg is invalid", "arg", arg)
			errList = append(errList, field.Invalid(
				fp.Child("buildArgs").Index(idx), arg, "must use a <key>=<value> format",
			))
		}
	}

	if !reference.TagRegexp.MatchString(in.Spec.CacheTag) {
		log.V(1).Info("Cache tag is invalid", "tag", in.Spec.CacheTag)
		errList = append(errList, field.Invalid(
			fp.Child("cacheTag"), in.Spec.CacheTag, fmt.Sprintf("must conform to the pattern %q", reference.TagRegexp),
		))
	}

	if in.Spec.CacheMode != CacheModeMax && in.Spec.CacheMode != CacheModeMin {
		log.V(1).Info("Cache mode is invalid", "mode", in.Spec.CacheMode)
		errList = append(errList, field.NotSupported(
			fp.Child("cacheMode"), in.Spec.CacheMode, []string{CacheModeMax, CacheModeMin},
		))
	}

	if errs := validateRegistryAuth(log, fp.Child("registryAuth"), in.Spec.RegistryAuth); errs != nil {
		errList = append(errList, errs...)
	}

	return invalidIfNotEmpty(ImageBuildKind, in.Name, errList)
}
