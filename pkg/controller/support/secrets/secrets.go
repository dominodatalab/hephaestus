package secrets

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

// exists only so it can overridden by tests with a fake client
var clientsetFunc = func(config *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(config)
}

func ReadSecrets(
	ctx context.Context,
	obj *hephv1.ImageBuild,
	log logr.Logger,
	cfg *rest.Config,
	scheme *runtime.Scheme,
) (map[string][]byte, error) {
	clientset, err := clientsetFunc(cfg)
	if err != nil {
		return map[string][]byte{}, fmt.Errorf("failure to get kubernetes client: %w", err)
	}
	v1 := clientset.CoreV1()

	// Extracts secrets into data to pass to buildkit
	secretsData := make(map[string][]byte)
	for _, secretRef := range obj.Spec.Secrets {
		secretClient := v1.Secrets(secretRef.Namespace)

		path := strings.Join([]string{secretRef.Namespace, secretRef.Name}, "/")
		log.Info("Finding secret", "path", path)
		fields := fields.SelectorFromSet(
			map[string]string{"metadata.namespace": secretRef.Namespace, "metadata.name": secretRef.Name})
		// prevent exfiltration of arbitrary secret values by using the presence of this label
		labels := labels.SelectorFromSet(map[string]string{hephv1.AccessLabel: "true"})
		secrets, err := secretClient.List(ctx,
			metav1.ListOptions{FieldSelector: fields.String(), LabelSelector: labels.String()})
		if err != nil {
			return map[string][]byte{}, fmt.Errorf("failure querying for secret %q: %w", path, err)
		}

		if len(secrets.Items) == 0 {
			return map[string][]byte{}, fmt.Errorf("secret %q unreadable or missing required label %q", path, hephv1.AccessLabel)
		}
		secret := &secrets.Items[0]

		// adopt the secret resource if hephaestus-owned is true to delete when ImageBuild is deleted
		if _, ok := secret.Labels[hephv1.OwnedLabel]; ok {
			log.Info("Taking ownership of secret", "owner", obj.Name, "secret", path)

			// non-fatal error thats logged but ignored
			if err = controllerutil.SetOwnerReference(obj, secret, scheme); err != nil {
				log.Info("Ignoring error taking ownership of secret", "secret", path, "error", err)
			} else if _, err = secretClient.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
				log.Info("Ignoring error taking ownership of secret", "secret", path, "error", err)
			}
		}

		if secretRef.MountPath != "" {
			path = secretRef.MountPath
		}

		// builds a path for the secret like {namespace}/{name}/{key} to avoid hash key collisions
		for filename, data := range secret.Data {
			name := strings.Join([]string{path, filename}, "/")
			secretsData[name] = data
			log.Info("Read secret bytes", "path", name, "bytes", len(data))
		}
	}

	return secretsData, nil
}
