package crd

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	"github.com/dominodatalab/hephaestus/deployments/crds"
	"github.com/dominodatalab/hephaestus/pkg/kubernetes"
)

// crdProcessor uses a CRD client to process the provided resource definition.
type crdProcessor func(context.Context, apixv1client.CustomResourceDefinitionInterface, *apixv1.CustomResourceDefinition) error

var (
	log = ctrlzap.New(ctrlzap.UseDevMode(true), ctrlzap.Encoder(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())))

	crdClientFn = crdClient

	applyFn = func(ctx context.Context, client apixv1client.CustomResourceDefinitionInterface, crd *apixv1.CustomResourceDefinition) error {
		log.Info("Fetching CRD", "Name", crd.Name)
		found, err := client.Get(ctx, crd.Name, metav1.GetOptions{})

		if apierrors.IsNotFound(err) {
			log.Info("CRD not found, creating", "Name", crd.Name)
			_, err = client.Create(ctx, crd, metav1.CreateOptions{})
		} else if err == nil {
			log.Info("CRD found, updating", "Name", crd.Name)
			crd.SetResourceVersion(found.ResourceVersion)
			_, err = client.Update(ctx, crd, metav1.UpdateOptions{})
		}

		return err
	}

	deleteFn = func(ctx context.Context, client apixv1client.CustomResourceDefinitionInterface, crd *apixv1.CustomResourceDefinition) error {
		log.Info("Deleting CRD", "Name", crd.Name)
		err := client.Delete(ctx, crd.Name, metav1.DeleteOptions{})

		if apierrors.IsNotFound(err) {
			log.Info("CRD not found, ignoring", "Name", crd.Name)
			return nil
		}

		return err
	}
)

// Apply will create or update all project CRDs inside a Kubernetes cluster.
//
// The latest available version of the CRD will be used to perform this operation.
func Apply(ctx context.Context, istioEnabled bool) error {
	return operate(ctx, applyFn, istioEnabled)
}

// Delete will remove all project CRDs from a Kubernetes cluster.
func Delete(ctx context.Context, istioEnabled bool) error {
	return operate(ctx, deleteFn, istioEnabled)
}

// Exists will check for the existence of a specific groupversion.
func Exists(gv metav1.GroupVersion) (bool, error) {
	config, err := kubernetes.RestConfig()
	if err != nil {
		return false, err
	}

	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return false, err
	}

	_, err = client.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// operate will read all available CRDS and apply state changes to the cluster using the processor func.
func operate(ctx context.Context, processor crdProcessor, istio bool) error {
	if istio {
		quit, err := waitForIstioSidecar()
		if err != nil {
			return err
		}

		defer quit()
	}

	log.Info("Loading all CRDs")

	definitions, err := crds.ReadAll()
	if err != nil {
		return err
	}

	client, err := crdClientFn()
	if err != nil {
		return err
	}

	for _, def := range definitions {
		bs, err := yaml.YAMLToJSON(def.Contents)
		if err != nil {
			return err
		}

		resource := new(apixv1.CustomResourceDefinition)
		if err := json.Unmarshal(bs, resource); err != nil {
			return err
		}

		if err := processor(ctx, client, resource); err != nil {
			return err
		}
	}

	return nil
}

// crdClient returns a client configured to work with custom resource definitions.
func crdClient() (apixv1client.CustomResourceDefinitionInterface, error) {
	log.Info("Initializing Kubernetes V1 CRD client")

	config, err := kubernetes.RestConfig()
	if err != nil {
		return nil, err
	}

	client, err := apixv1client.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return client.CustomResourceDefinitions(), nil
}
