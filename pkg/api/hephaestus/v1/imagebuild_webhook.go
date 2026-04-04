package v1

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var imagebuildlog = logf.Log.WithName("webhook").WithName("imagebuild")

var _ admission.Defaulter[*ImageBuild] = &ImageBuild{}

func (in *ImageBuild) Default(_ context.Context, obj *ImageBuild) error {
	log := imagebuildlog.WithName("defaulter").WithValues("imagebuild", client.ObjectKeyFromObject(obj))
	log.V(1).Info("Applying default values")
	return nil
}

var _ admission.Validator[*ImageBuild] = &ImageBuild{}

func (in *ImageBuild) ValidateCreate(_ context.Context, obj *ImageBuild) (admission.Warnings, error) {
	log := imagebuildlog.WithName("validator").WithValues("imagebuild", client.ObjectKeyFromObject(obj))
	log.V(1).Info("Using data from obj parameter",
		"name", obj.Name,
		"namespace", obj.Namespace,
		"context", obj.Spec.Context)
	return in.validateImageBuild("create", obj)
}

func (in *ImageBuild) ValidateUpdate(_ context.Context, _ *ImageBuild, newObj *ImageBuild) (admission.Warnings, error) {
	log := imagebuildlog.WithName("validator").WithValues(
		"action", "update",
		"name", newObj.Name,
		"namespace", newObj.Namespace,
		"spec.context", newObj.Spec.Context,
		"spec.hasDockerfileContents", newObj.Spec.DockerfileContents != "",
		"spec.imageCount", len(newObj.Spec.Images),
		"spec.Images", newObj.Spec.Images,
	)
	log.V(1).Info("Processing validation update")
	log.V(1).Info("Raw incoming object update", "object", fmt.Sprintf("%+v", newObj))
	return in.validateImageBuild("update", newObj)
}

func (in *ImageBuild) ValidateDelete(_ context.Context, _ *ImageBuild) (admission.Warnings, error) {
	return admission.Warnings{}, nil
}

func (in *ImageBuild) validateImageBuild(action string, obj *ImageBuild) (admission.Warnings, error) {
	log := imagebuildlog.WithName("validator").WithName(action).WithValues("imagebuild", client.ObjectKeyFromObject(obj))
	log.V(1).Info("Starting validation")

	var errList field.ErrorList
	fp := field.NewPath("spec")

	if strings.TrimSpace(obj.Spec.Context) == "" && strings.TrimSpace(obj.Spec.DockerfileContents) == "" {
		log.Info("Context and DockerfileContents are both blank")
		errList = append(errList, field.Required(fp.Child("context"), "must not be blank if "+
			fp.Child("dockerfileContents").String()+" is blank"))
	}
	if strings.TrimSpace(obj.Spec.Context) != "" {
		if _, err := url.ParseRequestURI(obj.Spec.Context); err != nil {
			log.Info("Context is not a valid URL")
			errList = append(errList, field.Invalid(fp.Child("context"), obj.Spec.Context, err.Error()))
		}
	}
	if errs := validateImages(log, fp.Child("images"), obj.Spec.Images); errs != nil {
		errList = append(errList, errs...)
	}
	for idx, arg := range obj.Spec.BuildArgs {
		if ss := strings.SplitN(arg, "=", 2); len(ss) != 2 || strings.TrimSpace(ss[0]) == "" {
			log.Info("Build arg is invalid", "arg", arg)
			errList = append(errList, field.Invalid(
				fp.Child("buildArgs").Index(idx), arg, "must use a <key>=<value> format",
			))
		}
	}
	if errs := validateRegistryAuth(log, fp.Child("registryAuth"), obj.Spec.RegistryAuth); errs != nil {
		errList = append(errList, errs...)
	}
	if strings.TrimSpace(obj.Spec.LogKey) == "" {
		log.Info("WARNING: Blank 'logKey' will preclude post-log processing")
	}

	return admission.Warnings{}, invalidIfNotEmpty(ImageBuildKind, obj.Name, errList)
}
