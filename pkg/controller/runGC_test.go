package controller

import (
	"context"
	"fmt"
	"testing"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	fakeHeph "github.com/dominodatalab/hephaestus/pkg/clientset/fake"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s "k8s.io/client-go/kubernetes"
	k8stesting "k8s.io/client-go/testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetNamespaces(t *testing.T) {
	fakeClientErr := fake.NewSimpleClientset()
	fakeClientErr.Fake.PrependReactor(
		"list", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("error listing namespaces")
		})

	tests := []struct {
		name        string
		client      k8s.Interface
		expectedLen int
		expectedErr error
	}{
		{
			name: "returns all namespaces",
			client: fake.NewSimpleClientset(&v1.Namespace{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "aloha",
					Namespace: "hawaii",
				},
				Spec:   v1.NamespaceSpec{},
				Status: v1.NamespaceStatus{},
			}, &v1.Namespace{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hola",
					Namespace: "Argentina",
				},
				Spec:   v1.NamespaceSpec{},
				Status: v1.NamespaceStatus{},
			}),
			expectedLen: 2,
		},
		{
			name:        "returns an error on List function",
			client:      fakeClientErr,
			expectedErr: fmt.Errorf("error listing namespaces"),
		},
	}
	{
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				ns, err := getAllNamespaces(tt.client)
				if tt.expectedErr != nil {
					assert.Error(t, err)
				} else {
					require.NoError(t, err)
					assert.Len(t, ns, tt.expectedLen)
				}
			})
		}
	}
}

func TestCleanUpCleanUpIBSuccess(t *testing.T) {
	fakeClient := fakeHeph.NewSimpleClientset()
	fakeClient.Fake.PrependReactor(
		"list", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := &hephv1.ImageBuildList{
				Items: []hephv1.ImageBuild{
					{
						Status: hephv1.ImageBuildStatus{
							Phase: "Failed",
						},
						ObjectMeta: metav1.ObjectMeta{Namespace: "aloha"},
					},
				},
			}
			return true, obj, nil
		})
	fakeClient.Fake.PrependReactor(
		"delete", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, nil
		})
	FakeGC := &ImageBuildGC{
		maxIBRetention: 0,
		hephClient:     fakeClient,
		namespaces:     []string{"aloha"},
	}
	err := FakeGC.CleanUpIBs(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)

	if len(fakeClient.Fake.Actions()) != 2 {
		t.Errorf("Expected one action, got %d", len(fakeClient.Fake.Actions()))
	}
}

func TestCleanUpIBsListErr(t *testing.T) {
	fakeClientErr := fakeHeph.NewSimpleClientset()
	fakeClientErr.Fake.PrependReactor(
		"list", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("error listing namespaces")
		})
	FakeGC := &ImageBuildGC{
		maxIBRetention: 1,
		hephClient:     fakeClientErr,
		namespaces:     nil,
	}
	err := FakeGC.CleanUpIBs(context.Background(), logr.Discard(), "aloha")
	require.Error(t, err)
	require.Contains(t, err.Error(), "error listing namespaces")
}

func TestCleanUpIBsNoIbs(t *testing.T) {
	fakeClientErr := fakeHeph.NewSimpleClientset()
	fakeClientErr.Fake.PrependReactor(
		"list", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := &hephv1.ImageBuildList{
				Items: []hephv1.ImageBuild{},
			}
			return true, obj, nil
		})
	FakeGC := &ImageBuildGC{
		maxIBRetention: 1,
		hephClient:     fakeClientErr,
		namespaces:     nil,
	}
	err := FakeGC.CleanUpIBs(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)
}

func TestCleanUpLessThanMaxRetention(t *testing.T) {
	fakeClientErr := fakeHeph.NewSimpleClientset()
	fakeClientErr.Fake.PrependReactor(
		"list", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := &hephv1.ImageBuildList{
				Items: []hephv1.ImageBuild{
					{
						Status: hephv1.ImageBuildStatus{
							Phase: "Failed",
						},
						ObjectMeta: metav1.ObjectMeta{Namespace: "aloha"},
					},
				},
			}
			return true, obj, nil
		})
	FakeGC := &ImageBuildGC{
		maxIBRetention: 1,
		hephClient:     fakeClientErr,
		namespaces:     []string{"aloha"},
	}
	err := FakeGC.CleanUpIBs(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)
}

func TestCleanUpMultipleBuildFailedDeletes(t *testing.T) {
	fakeClientErr := fakeHeph.NewSimpleClientset()
	fakeClientErr.Fake.PrependReactor(
		"list", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := &hephv1.ImageBuildList{
				Items: []hephv1.ImageBuild{
					{
						Status: hephv1.ImageBuildStatus{
							Phase: "Failed",
						},
						ObjectMeta: metav1.ObjectMeta{Namespace: "aloha"},
					},
				},
			}
			return true, obj, nil
		})
	fakeClientErr.Fake.PrependReactor(
		"delete", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("failed ib delete")
		})
	FakeGC := &ImageBuildGC{
		maxIBRetention: 0,
		hephClient:     fakeClientErr,
		namespaces:     []string{"aloha"},
	}
	err := FakeGC.CleanUpIBs(context.Background(), logr.Discard(), "aloha")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed ib delete")
}
