package component

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dominodatalab/controller-util/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgev1alpha1 "github.com/dominodatalab/hephaestus/pkg/api/forge/v1alpha1"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

const (
	forgeBuildIDAnnotation       = "imagebuilder.dominodatalab.com/build-id"
	forgeObjectStorageAnnotation = "hephaestus.dominodatalab.com/converted-object"
)

var errInconvertible = errors.New("cannot convert containerimagebuild object")

type ConversionShimComponent struct{}

func ConversionShim() *ConversionShimComponent {
	return &ConversionShimComponent{}
}

func (c ConversionShimComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	cib := ctx.Object.(*forgev1alpha1.ContainerImageBuild)

	/*
		check if named imagebuild already exists
	*/
	log.Info("Querying for ImageBuild with corresponding namespace/name")
	err := ctx.Client.Get(ctx, client.ObjectKey{Name: cib.Name, Namespace: cib.Namespace}, &hephv1.ImageBuild{})
	if err == nil {
		log.Info("ImageBuild exists, quitting reconcile")
		return ctrl.Result{}, nil
	}

	/*
		ensure logKey is present
	*/
	logKey := cib.Annotations[forgeBuildIDAnnotation]
	if strings.TrimSpace(logKey) == "" {
		err := fmt.Errorf("log key not in annotation %q: %w", forgeBuildIDAnnotation, errInconvertible)
		return ctrl.Result{}, err
	}

	/*
		capture original object in annotations
	*/
	bs, err := json.Marshal(cib)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot serialize containerimagebuild %q: %w", cib.Name, err)
	}
	annotations := map[string]string{forgeObjectStorageAnnotation: string(bs)}

	/*
		convert registry credentials to new format
	*/
	var auths []hephv1.RegistryCredentials
	for _, reg := range cib.Spec.Registries {
		auth := hephv1.RegistryCredentials{
			Server:        reg.Server,
			CloudProvided: pointer.Bool(reg.DynamicCloudCredentials),
		}

		if reg.BasicAuth.IsInline() {
			auth.BasicAuth = &hephv1.BasicAuthCredentials{
				Username: reg.BasicAuth.Username,
				Password: reg.BasicAuth.Password,
			}
		}
		if reg.BasicAuth.IsSecret() {
			auth.Secret = &hephv1.SecretCredentials{
				Name:      reg.BasicAuth.SecretName,
				Namespace: reg.BasicAuth.SecretNamespace,
			}
		}

		auths = append(auths)
	}

	/*
		compose final object and submit to api for actual processing
	*/
	imageBuild := &hephv1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cib.Name,
			Namespace:   cib.Namespace,
			Labels:      cib.Labels,
			Annotations: annotations,
		},
		Spec: hephv1.ImageBuildSpec{
			Context:      cib.Spec.Context,
			Images:       []string{cib.Spec.ImageName},
			BuildArgs:    cib.Spec.BuildArgs,
			LogKey:       logKey,
			RegistryAuth: auths,
		},
	}

	log.Info("Creating imagebuild")
	if err := ctx.Client.Create(ctx, imageBuild); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
