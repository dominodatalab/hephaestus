package v1

import (
	"net/url"
	"regexp"
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

const maxAdditionalContextNameLength = 63

var (
	// additionalContextNameRE must be kept in sync with the pattern marker on AdditionalContext.Name. It requires a
	// lowercase name that starts with a letter and is a valid image reference path component without trailing
	// separators.
	additionalContextNameRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	sha256HexRE             = regexp.MustCompile(`^[0-9a-f]{64}$`)

	reservedContextNames = map[string]struct{}{"context": {}, "dockerfile": {}}
)

func validateAdditionalContexts(log logr.Logger, fp *field.Path, contexts []AdditionalContext) field.ErrorList {
	var errs field.ErrorList

	seen := make(map[string]struct{}, len(contexts))
	for idx, ac := range contexts {
		fp := fp.Index(idx)

		if nameErrs := validateAdditionalContextName(log, fp.Child("name"), ac.Name); len(nameErrs) > 0 {
			errs = append(errs, nameErrs...)
		} else {
			if _, reserved := reservedContextNames[ac.Name]; reserved {
				errs = append(errs, field.Invalid(fp.Child("name"), ac.Name, "is a reserved name"))
			}
			if _, dup := seen[ac.Name]; dup {
				errs = append(errs, field.Duplicate(fp.Child("name"), ac.Name))
			}
			seen[ac.Name] = struct{}{}
		}

		errs = append(errs, validateAdditionalContextURL(fp.Child("url"), ac)...)

		if strings.TrimSpace(ac.SHA256) == "" {
			errs = append(errs, field.Required(fp.Child("sha256"), "must not be blank"))
		} else if !sha256HexRE.MatchString(ac.SHA256) {
			errs = append(errs, field.Invalid(fp.Child("sha256"), ac.SHA256, "must be 64 lowercase hex characters"))
		}

		if ac.MaxSizeBytes < 1 {
			errs = append(errs, field.Invalid(fp.Child("maxSizeBytes"), ac.MaxSizeBytes, "must be greater than 0"))
		}

		if ts := ac.TokenSecret; ts != nil {
			if strings.TrimSpace(ts.Name) == "" {
				errs = append(errs, field.Required(fp.Child("tokenSecret", "name"), "must not be blank"))
			}
			if strings.TrimSpace(ts.Key) == "" {
				errs = append(errs, field.Required(fp.Child("tokenSecret", "key"), "must not be blank"))
			}
		}
	}

	return errs
}

// validateAdditionalContextName checks that name is usable as a BuildKit named context. BuildKit parses the name as an
// image reference (which rejects uppercase characters) and Dockerfile stage names are lowercase.
func validateAdditionalContextName(log logr.Logger, fp *field.Path, name string) field.ErrorList {
	if strings.TrimSpace(name) == "" {
		log.V(1).Info("Additional context name is blank")

		return field.ErrorList{field.Required(fp, "must not be blank")}
	}

	if len(name) > maxAdditionalContextNameLength {
		return field.ErrorList{field.TooLong(fp, name, maxAdditionalContextNameLength)}
	}

	if !additionalContextNameRE.MatchString(name) {
		return field.ErrorList{field.Invalid(fp, name, "must be lowercase, start with a letter, contain only "+
			"letters, digits and the separators '.', '_', '__' or '-', and must not end with a separator")}
	}

	if _, err := reference.ParseNormalizedNamed(name); err != nil {
		return field.ErrorList{field.Invalid(fp, name, "must be a valid image reference name: "+err.Error())}
	}

	return nil
}

func validateAdditionalContextURL(fp *field.Path, ac AdditionalContext) field.ErrorList {
	if strings.TrimSpace(ac.URL) == "" {
		return field.ErrorList{field.Required(fp, "must not be blank")}
	}

	u, err := url.Parse(ac.URL)
	if err != nil {
		return field.ErrorList{field.Invalid(fp, ac.URL, err.Error())}
	}

	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return field.ErrorList{field.Invalid(fp, ac.URL, "must be an absolute http or https URL")}
	}

	// the token is sent as a bearer credential, so never allow it to travel over plain http.
	if ac.TokenSecret != nil && u.Scheme != "https" {
		return field.ErrorList{field.Invalid(fp, ac.URL, "must use https when tokenSecret is set")}
	}

	return nil
}

func invalidIfNotEmpty(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(SchemeGroupVersion.WithKind(kind).GroupKind(), name, errs)
}
