package component

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dominodatalab/controller-util/core"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgev1alpha1 "github.com/dominodatalab/hephaestus/pkg/api/forge/v1alpha1"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

const (
	forgeLogKeyAnnotation        = "logKey"
	forgeLastBuildAnnotation     = "imagebuilder.dominodatalab.com/last-image"
	forgeObjectStorageAnnotation = "hephaestus.dominodatalab.com/converted-object"
)

var ErrInconvertible = errors.New("cannot convert containerimagebuild object")

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
		pass annotations with original object
	*/
	bs, err := json.Marshal(cib)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot serialize containerimagebuild %q: %w", cib.Name, err)
	}
	annotations := map[string]string{forgeObjectStorageAnnotation: string(bs)}
	for k, v := range cib.Annotations {
		annotations[k] = v
	}

	/*
		ensure logKey is present
	*/
	logKey := cib.Annotations[forgeLogKeyAnnotation]
	if strings.TrimSpace(logKey) == "" {
		err := fmt.Errorf("%q not in annotations %v: %w", forgeLogKeyAnnotation, cib.Annotations, ErrInconvertible)
		return ctrl.Result{}, err
	}

	/*
		compute cache imports
	*/
	var cacheImports []string
	if ref, ok := cib.Annotations[forgeLastBuildAnnotation]; ok {
		cacheImports = []string{ref}
	}

	/*
		convert push registries into fully-qualified image paths
	*/
	var images []string
	for _, reg := range cib.Spec.PushRegistries {
		images = append(images, filepath.Join(reg, cib.Spec.ImageName))
	}

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

		auths = append(auths, auth)
	}

	/*
		conditionally override status update queue
	*/
	var amqpOverrides *hephv1.ImageBuildAMQPOverrides
	if queue := cib.Spec.MessageQueueName; queue != "" {
		amqpOverrides = &hephv1.ImageBuildAMQPOverrides{
			QueueName: queue,
		}
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
			Context:                cib.Spec.Context,
			BuildArgs:              cib.Spec.BuildArgs,
			DisableLocalBuildCache: cib.Spec.DisableBuildCache,
			// TODO: it can take upwards of 5m to export inline layer cache for huge images
			// 	we need to possibly support `--target` before using this feature.
			DisableCacheLayerExport: true,
			ImportRemoteBuildCache:  cacheImports,
			Images:                  images,
			LogKey:                  logKey,
			RegistryAuth:            auths,
			AMQPOverrides:           amqpOverrides,
		},
	}

	log.Info("Creating imagebuild")
	if err := ctx.Client.Create(ctx, imageBuild); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (c ConversionShimComponent) Finalize(ctx *core.Context) (ctrl.Result, bool, error) {
	log := ctx.Log
	cib := ctx.Object.(*forgev1alpha1.ContainerImageBuild)
	ib := &hephv1.ImageBuild{}

	log.Info("Querying for ImageBuild with corresponding namespace/name")
	if err := ctx.Client.Get(ctx, client.ObjectKey{Name: cib.Name, Namespace: cib.Namespace}, ib); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Aborting reconcile, ImageBuild does not exist")
			return ctrl.Result{}, true, nil
		}

		return ctrl.Result{}, false, err
	}

	log.Info("Deleting ImageBuild")
	if err := ctx.Client.Delete(ctx, ib); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("failed to delete associated imagebuild: %w", err)
	}

	return ctrl.Result{}, true, nil
}
