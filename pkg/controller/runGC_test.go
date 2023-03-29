package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	fakeHeph "github.com/dominodatalab/hephaestus/pkg/clientset/fake"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
	ctx := context.Background()
	ibv1 := fakeClient.HephaestusV1().ImageBuilds("aloha")

	ib := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseFailed,
		},
		ObjectMeta: metav1.ObjectMeta{Namespace: "aloha"},
	}

	ibKeep := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseInitializing,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "keep-me",
			Namespace: "aloha",
		},
	}

	ibv1.Create(ctx, &ib, metav1.CreateOptions{})
	ibv1.Create(ctx, &ibKeep, metav1.CreateOptions{})

	ogIbList, listErr := ibv1.List(ctx, metav1.ListOptions{})
	assert.NoError(t, listErr)
	assert.Len(t, ogIbList.Items, 2)

	FakeGC := &ImageBuildGC{
		maxIBRetention: 0,
		hephClient:     fakeClient,
		namespaces:     []string{"aloha"},
	}
	err := FakeGC.GCImageBuilds(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)

	ibList, secondCallListErr := ibv1.List(ctx, metav1.ListOptions{})
	assert.NoError(t, secondCallListErr)
	assert.Len(t, ibList.Items, 1)
}

func TestCleanUpCleanUpIBSuccessSortOrder(t *testing.T) {
	fakeClient := fakeHeph.NewSimpleClientset()
	ibv1 := fakeClient.HephaestusV1().ImageBuilds("aloha")
	oldIb := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseSucceeded,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "delete-me-oldest-ib",
			Namespace:         "aloha",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
	}
	newIb := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseSucceeded,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "keep-me-newest-ib",
			Namespace:         "aloha",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
	}

	ctx := context.Background()
	ibv1.Create(ctx, &oldIb, metav1.CreateOptions{})
	ibv1.Create(ctx, &newIb, metav1.CreateOptions{})

	ogIbs, listErr := ibv1.List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Len(t, ogIbs.Items, 2)

	FakeGC := &ImageBuildGC{
		maxIBRetention: 1,
		hephClient:     fakeClient,
		namespaces:     []string{"aloha"},
	}

	err := FakeGC.GCImageBuilds(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)

	ibs, err := ibv1.List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	assert.Len(t, ibs.Items, 1)
	assert.Equal(t, ibs.Items[0].Name, "keep-me-newest-ib")
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
	err := FakeGC.GCImageBuilds(context.Background(), logr.Discard(), "aloha")
	require.Error(t, err)
	require.Contains(t, err.Error(), "error listing namespaces")
}

func TestCleanUpIBsNoIbs(t *testing.T) {
	fakeClientErr := fakeHeph.NewSimpleClientset()
	FakeGC := &ImageBuildGC{
		maxIBRetention: 1,
		hephClient:     fakeClientErr,
		namespaces:     nil,
	}
	err := FakeGC.GCImageBuilds(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)
}

func TestCleanUpLessThanMaxRetention(t *testing.T) {
	fakeClient := fakeHeph.NewSimpleClientset()
	ibv1 := fakeClient.HephaestusV1().ImageBuilds("aloha")
	ib := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseFailed,
		},
		ObjectMeta: metav1.ObjectMeta{Namespace: "aloha"},
	}

	ctx := context.Background()
	ibv1.Create(ctx, &ib, metav1.CreateOptions{})

	ogIbs, listErr := ibv1.List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Len(t, ogIbs.Items, 1)

	FakeGC := &ImageBuildGC{
		maxIBRetention: 1,
		hephClient:     fakeClient,
		namespaces:     []string{"aloha"},
	}
	err := FakeGC.GCImageBuilds(context.Background(), logr.Discard(), "aloha")
	require.NoError(t, err)

	ibs, secondCallListErr := ibv1.List(ctx, metav1.ListOptions{})
	require.NoError(t, secondCallListErr)
	assert.Len(t, ibs.Items, 1)
}

func TestCleanUpMultipleBuildFailedDeletes(t *testing.T) {
	fakeClient := fakeHeph.NewSimpleClientset()
	ibv1 := fakeClient.HephaestusV1().ImageBuilds("aloha")
	ib := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseFailed,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "thing1",
			Namespace: "aloha",
		},
	}

	ib2 := hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseFailed,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "thing2",
			Namespace: "aloha",
		},
	}

	ctx := context.Background()
	ibv1.Create(ctx, &ib, metav1.CreateOptions{})
	ibv1.Create(ctx, &ib2, metav1.CreateOptions{})

	ogIbs, listErr := ibv1.List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Len(t, ogIbs.Items, 2)

	fakeClient.Fake.PrependReactor(
		"delete", "imagebuilds", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("failed ib delete")
		})
	FakeGC := &ImageBuildGC{
		maxIBRetention: 0,
		hephClient:     fakeClient,
		namespaces:     []string{"aloha"},
	}
	err := FakeGC.GCImageBuilds(context.Background(), logr.Discard(), "aloha")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed ib delete")
}
