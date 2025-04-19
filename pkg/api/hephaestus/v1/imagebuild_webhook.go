package v1

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var imagebuildlog = logf.Log.WithName("webhook").WithName("imagebuild")

var _ webhook.CustomDefaulter = &ImageBuild{}

func (in *ImageBuild) Default(context.Context, runtime.Object) error {
	log := imagebuildlog.WithName("defaulter").WithValues("imagebuild", client.ObjectKeyFromObject(in))
	log.V(1).Info("Applying default values")
	return nil
}

var _ webhook.CustomValidator = &ImageBuild{}

func (in *ImageBuild) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if newObj, ok := obj.(*ImageBuild); ok {
		log := imagebuildlog.WithName("validator").WithValues("imagebuild", client.ObjectKeyFromObject(in))
		log.V(1).Info("Using data from obj parameter",
			"name", newObj.Name,
			"namespace", newObj.Namespace,
			"context", newObj.Spec.Context)
	}
	return in.validateImageBuild("create", obj)
}

//nolint:lll
func (in *ImageBuild) ValidateUpdate(_ context.Context, obj runtime.Object, _ runtime.Object) (admission.Warnings, error) {
	if newObj, ok := obj.(*ImageBuild); ok {
		log := imagebuildlog.WithName("validator").WithValues(
			"action", "create",
			"name", newObj.Name,
			"namespace", newObj.Namespace,
			"spec.context", newObj.Spec.Context,
			"spec.hasDockerfileContents", newObj.Spec.DockerfileContents != "",
			"spec.imageCount", len(newObj.Spec.Images),
			"spec.Images", newObj.Spec.Images,
			"object_type", fmt.Sprintf("%T", obj),
		)
		log.V(1).Info("Processing validation update ")
		log.V(1).Info("Raw incoming object update", "object", fmt.Sprintf("%+v", obj))
	}

	return in.validateImageBuild("update", obj)
}

func (in *ImageBuild) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return admission.Warnings{}, nil
}

func (in *ImageBuild) validateImageBuild(action string, obj runtime.Object) (admission.Warnings, error) {
	log := imagebuildlog.WithName("validator").WithName(action).WithValues("imagebuild", client.ObjectKeyFromObject(in))
	log.V(1).Info("Starting validation")

	var errList field.ErrorList
	fp := field.NewPath("spec")

	if newObj, ok := obj.(*ImageBuild); ok {
		if strings.TrimSpace(newObj.Spec.Context) == "" && strings.TrimSpace(newObj.Spec.DockerfileContents) == "" {
			log.Info("Context and DockerfileContents are both blank")
			errList = append(errList, field.Required(fp.Child("context"), "must not be blank if "+
				fp.Child("dockerfileContents").String()+" is blank"))
		}

		if strings.TrimSpace(newObj.Spec.Context) != "" {
			if _, err := url.ParseRequestURI(newObj.Spec.Context); err != nil {
				log.Info("Context is not a valid URL")
				errList = append(errList, field.Invalid(fp.Child("context"), newObj.Spec.Context, err.Error()))
			}
		}

		if errs := validateImages(log, fp.Child("images"), newObj.Spec.Images); errs != nil {
			errList = append(errList, errs...)
		}

		for idx, arg := range newObj.Spec.BuildArgs {
			if ss := strings.SplitN(arg, "=", 2); len(ss) != 2 || strings.TrimSpace(ss[0]) == "" {
				log.Info("Build arg is invalid", "arg", arg)
				errList = append(errList, field.Invalid(
					fp.Child("buildArgs").Index(idx), arg, "must use a <key>=<value> format",
				))
			}
		}

		if errs := validateRegistryAuth(log, fp.Child("registryAuth"), newObj.Spec.RegistryAuth); errs != nil {
			errList = append(errList, errs...)
		}

		if strings.TrimSpace(newObj.Spec.LogKey) == "" {
			log.Info("WARNING: Blank 'logKey' will preclude post-log processing")
		}
		return admission.Warnings{}, invalidIfNotEmpty(ImageBuildKind, newObj.Name, errList)
	}

	return admission.Warnings{}, nil
}
