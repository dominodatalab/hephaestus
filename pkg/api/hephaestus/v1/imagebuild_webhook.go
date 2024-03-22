package v1

import (
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var imagebuildlog = logf.Log.WithName("webhook").WithName("imagebuild")

var _ webhook.Defaulter = &ImageBuild{}

func (in *ImageBuild) Default() {
	log := imagebuildlog.WithName("defaulter").WithValues("imagebuild", client.ObjectKeyFromObject(in))
	log.V(1).Info("Applying default values")
}

var _ webhook.Validator = &ImageBuild{}

func (in *ImageBuild) ValidateCreate() (admission.Warnings, error) {
	return in.validateImageBuild("create")
}

func (in *ImageBuild) ValidateUpdate(runtime.Object) (admission.Warnings, error) {
	return in.validateImageBuild("update")
}

func (in *ImageBuild) ValidateDelete() (admission.Warnings, error) {
	return admission.Warnings{}, nil
}

func (in *ImageBuild) validateImageBuild(action string) (admission.Warnings, error) {
	log := imagebuildlog.WithName("validator").WithName(action).WithValues("imagebuild", client.ObjectKeyFromObject(in))
	log.V(1).Info("Starting validation")

	var errList field.ErrorList
	fp := field.NewPath("spec")

	if strings.TrimSpace(in.Spec.Context) == "" && strings.TrimSpace(in.Spec.DockerfileContents) == "" {
		log.V(1).Info("Context and DockerfileContents are both blank")
		errList = append(errList, field.Required(fp.Child("context or dockerFileContents"), "must not be blank"))
	}

	if strings.TrimSpace(in.Spec.Context) != "" {
		if _, err := url.ParseRequestURI(in.Spec.Context); err != nil {
			log.V(1).Info("Context is not a valid URL")
			errList = append(errList, field.Invalid(fp.Child("context"), in.Spec.Context, err.Error()))
		}
	}

	if errs := validateImages(log, fp.Child("images"), in.Spec.Images); errs != nil {
		errList = append(errList, errs...)
	}

	for idx, arg := range in.Spec.BuildArgs {
		if ss := strings.SplitN(arg, "=", 2); len(ss) != 2 || strings.TrimSpace(ss[0]) == "" {
			log.V(1).Info("Build arg is invalid", "arg", arg)
			errList = append(errList, field.Invalid(
				fp.Child("buildArgs").Index(idx), arg, "must use a <key>=<value> format",
			))
		}
	}

	if errs := validateRegistryAuth(log, fp.Child("registryAuth"), in.Spec.RegistryAuth); errs != nil {
		errList = append(errList, errs...)
	}

	if strings.TrimSpace(in.Spec.LogKey) == "" {
		log.Info("WARNING: Blank 'logKey' will preclude post-log processing")
	}

	return admission.Warnings{}, invalidIfNotEmpty(ImageBuildKind, in.Name, errList)
}
