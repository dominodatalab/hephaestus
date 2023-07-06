package secrets

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

// NOTE: this doesn't cover k8s permissioning for secret access
func TestReadSecrets(t *testing.T) {
	for name, tc := range map[string]struct {
		RequestedSecrets []hephv1.SecretReference
		ClientResponse   []runtime.Object
		Want             map[string][]byte
		WantError        bool
	}{
		"returns data for secret in same namespace": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "domino-compute", Name: "foo"}},
			ClientResponse: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "domino-compute",
					Name:      "foo",
					Labels:    map[string]string{"hephaestus-accessible": "true"},
				},
				Data: map[string][]byte{"bar": []byte("hello")},
			}},
			Want: map[string][]byte{"domino-compute/foo/bar": []byte("hello")},
		},
		"returns empty data for empty secret": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "domino-compute", Name: "foo"}},
			ClientResponse: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "domino-compute",
					Name:      "foo",
					Labels:    map[string]string{"hephaestus-accessible": "true"},
				},
			}},
			Want: map[string][]byte{},
		},
		"returns all data within a secret, including multiline data": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "domino-compute", Name: "groups"}},
			ClientResponse: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "domino-compute",
					Name:      "groups",
					Labels:    map[string]string{"hephaestus-accessible": "true"},
				},
				Data: map[string][]byte{
					"atcq":    []byte("q-tip, phife, ali shaheed, jarobi"),
					"wu-tang": []byte("rza\ngza\nodb\ninspectah deck\nu-god\nghost face\nmethod man"),
				},
			}},
			Want: map[string][]byte{
				"domino-compute/groups/atcq":    []byte("q-tip, phife, ali shaheed, jarobi"),
				"domino-compute/groups/wu-tang": []byte("rza\ngza\nodb\ninspectah deck\nu-god\nghost face\nmethod man"),
			},
		},
		"returns data for secrets in different namespace": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "foo", Name: "bar"}},
			ClientResponse: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "foo",
					Name:      "bar",
					Labels:    map[string]string{"hephaestus-accessible": "true"},
				},
				Data: map[string][]byte{"baz": []byte("hello")},
			}},
			Want: map[string][]byte{"foo/bar/baz": []byte("hello")},
		},
		"uses namespace to differentiate secrets": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "domino-test", Name: "foo"}},
			ClientResponse: []runtime.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "domino-compute",
						Name:      "foo",
						Labels:    map[string]string{"hephaestus-accessible": "true"},
					},
					Data: map[string][]byte{"bar": []byte("hello")},
				},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "domino-test",
						Name:      "foo",
						Labels:    map[string]string{"hephaestus-accessible": "true"},
					},
					Data: map[string][]byte{"bar": []byte("goodbye")},
				},
			},
			Want: map[string][]byte{"domino-test/foo/bar": []byte("goodbye")},
		},
		"errors for missing secrets": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "foo", Name: "bar"}},
			ClientResponse:   []runtime.Object{},
			WantError:        true,
		},
		"requires secrets to have hephaestus-accessible label": {
			RequestedSecrets: []hephv1.SecretReference{{Namespace: "foo", Name: "bar"}},
			ClientResponse: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "foo",
					Name:      "bar",
				},
				Data: map[string][]byte{"baz": []byte("hello")},
			}},
			WantError: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Assume a static domino-compute namespace for all ImageBuild requests
			img := &hephv1.ImageBuild{
				Status: hephv1.ImageBuildStatus{
					Phase: hephv1.PhaseInitializing,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "image-build-request",
					Namespace: "domino-compute",
				},
				Spec: hephv1.ImageBuildSpec{
					Secrets: tc.RequestedSecrets,
				},
			}

			clientsetFunc = func(*rest.Config) (kubernetes.Interface, error) {
				return fake.NewSimpleClientset(tc.ClientResponse...), nil
			}

			secretData, err := ReadSecrets(context.Background(), img, logr.Discard(), nil, nil)

			if tc.WantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.Want, secretData)
			}
		})
	}
}

func TestReadSecretsTakesOwnership(t *testing.T) {
	for name, tc := range map[string]struct {
		RequestedSecret     hephv1.SecretReference
		ReturnedSecret      *corev1.Secret
		Want                map[string][]byte
		WantImageBuildOwner bool
	}{
		"does not change owner by default": {
			RequestedSecret: hephv1.SecretReference{Namespace: "domino-compute", Name: "foo"},
			ReturnedSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "domino-compute",
					Name:      "foo",
					Labels:    map[string]string{"hephaestus-accessible": "true"},
				},
				Data: map[string][]byte{"bar": []byte("hello")},
			},
			Want: map[string][]byte{"domino-compute/foo/bar": []byte("hello")},
		},
		"updates owner reference to ImageBuild when hephaestus-owned label set": {
			RequestedSecret: hephv1.SecretReference{Namespace: "domino-compute", Name: "foo"},
			ReturnedSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "domino-compute",
					Name:      "foo",
					Labels: map[string]string{
						"hephaestus-owned":      "true",
						"hephaestus-accessible": "true",
					},
				},
				Data: map[string][]byte{"bar": []byte("hello")},
			},
			Want:                map[string][]byte{"domino-compute/foo/bar": []byte("hello")},
			WantImageBuildOwner: true,
		},
		"does not update owner references across namespaces, but still returns data": {
			RequestedSecret: hephv1.SecretReference{Namespace: "domino-other", Name: "foo"},
			ReturnedSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "domino-other",
					Name:      "foo",
					Labels: map[string]string{
						"hephaestus-owned":      "true",
						"hephaestus-accessible": "true",
					},
				},
				Data: map[string][]byte{"bar": []byte("hello")},
			},
			Want: map[string][]byte{"domino-other/foo/bar": []byte("hello")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Assume a static domino-compute namespace for all ImageBuild requests
			img := &hephv1.ImageBuild{
				Status: hephv1.ImageBuildStatus{
					Phase: hephv1.PhaseInitializing,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "image-build-request",
					Namespace: "domino-compute",
				},
				Spec: hephv1.ImageBuildSpec{
					Secrets: []hephv1.SecretReference{tc.RequestedSecret},
				},
			}

			simpleClient := fake.NewSimpleClientset(tc.ReturnedSecret)
			clientsetFunc = func(*rest.Config) (kubernetes.Interface, error) { return simpleClient, nil }

			schema, _ := hephv1.SchemeBuilder.Build()
			secretData, err := ReadSecrets(context.Background(), img, logr.Discard(), nil, schema)

			assert.NoError(t, err)
			assert.Equal(t, tc.Want, secretData)

			updatedSecret, err := simpleClient.CoreV1().Secrets(tc.RequestedSecret.Namespace).Get(context.Background(), tc.RequestedSecret.Name, metav1.GetOptions{})
			assert.NoError(t, err)

			if !tc.WantImageBuildOwner {
				assert.Empty(t, updatedSecret.OwnerReferences)
			} else {
				assert.Equal(t, "ImageBuild", updatedSecret.OwnerReferences[0].Kind)
				assert.Equal(t, "image-build-request", updatedSecret.OwnerReferences[0].Name)
			}
		})
	}
}
