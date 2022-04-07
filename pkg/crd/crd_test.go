package crd

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apixv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var objectMetaName = "imagebuilds.hephaestus.dominodatalab.com"

func TestApply(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()

		fakeClient.PrependReactor("get", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), action.GetSubresource())
		})

		created := false
		fakeClient.PrependReactor("create", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			created = true
			return true, nil, nil
		})

		t.Cleanup(overrideCRDClient(fakeClient))

		require.NoError(t, Apply(context.Background(), false))
		assert.True(t, created, "New CRD was not created")
	})

	t.Run("update", func(t *testing.T) {
		resourceVersion := "12345"
		fakeClient := fake.NewSimpleClientset(&apixv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:            objectMetaName,
				ResourceVersion: resourceVersion,
			},
		})

		updated := false
		fakeClient.PrependReactor("update", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			updateAction := action.(k8stesting.UpdateAction)
			obj := updateAction.GetObject().(*apixv1.CustomResourceDefinition)
			assert.Equalf(t, resourceVersion, obj.ResourceVersion,
				"ResourceVersion was not passed through on update; received %v, expected %v", obj.ResourceVersion, resourceVersion)

			updated = true
			return true, nil, nil
		})

		t.Cleanup(overrideCRDClient(fakeClient))

		require.NoError(t, Apply(context.Background(), false))
		assert.True(t, updated, "Existing CRD was not updated")
	})

	t.Run("error", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()

		expected := apierrors.NewInternalError(errors.New("this is an error"))
		fakeClient.PrependReactor("get", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, expected
		})

		t.Cleanup(overrideCRDClient(fakeClient))

		err := Apply(context.Background(), false)
		assert.Equalf(t, expected, err, "Received error %v did not match %v", err, expected)
	})
}

func TestDelete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset(&apixv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: objectMetaName,
			},
		})

		deleted := false
		fakeClient.PrependReactor("delete", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			deleted = true
			return true, nil, nil
		})

		t.Cleanup(overrideCRDClient(fakeClient))

		require.NoError(t, Delete(context.Background(), false))
		assert.True(t, deleted, "Existing CRD was not deleted")
	})

	t.Run("not_found", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()
		fakeClient.PrependReactor("get", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), action.GetSubresource())
		})

		t.Cleanup(overrideCRDClient(fakeClient))

		assert.NoError(t, Delete(context.Background(), false), "Delete failed when CRD not found")
	})

	t.Run("error", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()
		expected := apierrors.NewInternalError(errors.New("this is an error"))
		fakeClient.PrependReactor("delete", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, expected
		})

		t.Cleanup(overrideCRDClient(fakeClient))

		err := Delete(context.Background(), false)
		assert.Equalf(t, expected, err, "Received error %v did not match %v", err, expected)
	})
}

func overrideCRDClient(clientset *fake.Clientset) (reset func()) {
	origFn := crdClientFn
	reset = func() {
		crdClientFn = origFn
	}

	crdClientFn = func() (apixv1client.CustomResourceDefinitionInterface, error) {
		return clientset.ApiextensionsV1().CustomResourceDefinitions(), nil
	}

	return
}

func init() {
	log = zap.New(zap.WriteTo(io.Discard))
}
