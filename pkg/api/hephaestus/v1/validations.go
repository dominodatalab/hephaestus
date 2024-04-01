package v1

import (
	"strings"

	"github.com/distribution/reference"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func validateImages(log logr.Logger, fp *field.Path, images []string) (errs field.ErrorList) {
	if len(images) == 0 {
		log.V(1).Info("No images provided")
		errs = append(errs, field.Required(fp, "must contain at least 1 image"))

		return
	}

	for _, ref := range images {
		if _, err := reference.ParseAnyReference(ref); err != nil {
			log.V(1).Info("Image reference failed to parse", "ref", ref)
			errs = append(errs, field.Invalid(fp, ref, err.Error()))
		}
	}

	return
}

func validateRegistryAuth(log logr.Logger, fp *field.Path, registryAuth []RegistryCredentials) field.ErrorList {
	var errs field.ErrorList

	for idx, auth := range registryAuth {
		fp := fp.Index(idx)

		if strings.TrimSpace(auth.Server) == "" {
			log.V(1).Info("Registry credential server is blank")
			errs = append(errs, field.Required(fp.Child("server"), "must not be blank"))
		}

		ba := auth.BasicAuth != nil
		sa := auth.Secret != nil

		if ba && sa {
			log.V(1).Info("Multiple registry credential sources provided")
			errs = append(errs, field.Forbidden(fp, "cannot specify more than 1 credential source"))

			continue
		}

		switch {
		case ba:
			if strings.TrimSpace(auth.BasicAuth.Username) == "" {
				log.V(1).Info("Registry credential basic auth username is missing")
				errs = append(errs, field.Required(fp.Child("basicAuth", "username"), "must not be blank"))
			}
			if strings.TrimSpace(auth.BasicAuth.Password) == "" {
				log.V(1).Info("Registry credentials basic auth password is missing")
				errs = append(errs, field.Required(fp.Child("basicAuth", "password"), "must not be blank"))
			}
		case sa:
			if strings.TrimSpace(auth.Secret.Name) == "" {
				log.V(1).Info("Registry credentials secret name is missing")
				errs = append(errs, field.Required(fp.Child("secret", "name"), "must not be blank"))
			}
			if strings.TrimSpace(auth.Secret.Namespace) == "" {
				log.V(1).Info("Registry credentials secret namespace is missing")
				errs = append(errs, field.Required(fp.Child("secret", "namespace"), "must not be blank"))
			}
		default:
			log.V(1).Info("No registry credential sources provided")
		}
	}

	return errs
}

func invalidIfNotEmpty(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(SchemeGroupVersion.WithKind(kind).GroupKind(), name, errs)
}
