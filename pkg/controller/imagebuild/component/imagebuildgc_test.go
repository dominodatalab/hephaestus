package component

import (
	"context"
	"errors"
	"testing"
	"time"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var ibGVR = schema.GroupVersionResource{
	Group:    hephv1.SchemeGroupVersion.Group,
	Version:  hephv1.SchemeGroupVersion.Version,
	Resource: "imagebuilds",
}

var ibGVK = schema.GroupVersionKind{
	Group:   hephv1.SchemeGroupVersion.Group,
	Version: hephv1.SchemeGroupVersion.Version,
	Kind:    hephv1.ImageBuildKind,
}

func TestGC(t *testing.T) {
	now := time.Now()
	ibSuccess := ib("Success", "aloha", now)

	ibKeep := ib("keep", "aloha", now)
	ibKeep.Status.Phase = hephv1.PhaseInitializing

	ibFailed := ib("Failed", "aloha2", now)
	ibFailed.Status.Phase = hephv1.PhaseFailed

	for _, tt := range []struct {
		name       string
		namespaces []string
		expected   []invocation
	}{
		{
			"All",
			[]string{""},
			[]invocation{
				invokeList(""),
				invokeDelete(ibFailed),
				invokeDelete(ibSuccess),
			},
		},
		{
			"Per namspace",
			[]string{"aloha", "aloha2", "aloha3"},
			[]invocation{
				invokeList("aloha"),
				invokeDelete(ibSuccess),
				invokeList("aloha2"),
				invokeDelete(ibFailed),
				invokeList("aloha3"),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schema, _ := hephv1.SchemeBuilder.Build()
			fakeClient := fake.NewClientBuilder().WithScheme(schema).WithObjects(&ibSuccess, &ibKeep, &ibFailed).Build()
			recorder := newRecorder(fakeClient)

			ctx := context.Background()

			gc := &ImageBuildGC{
				HistoryLimit: 0,
				Client:       recorder.client,
				Namespaces:   tt.namespaces,
			}
			err := gc.GC(ctx)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}

			checkInvokes(t, tt.expected, recorder.invokes)
		})
	}
}

func TestGCSortOrder(t *testing.T) {
	now := time.Now()

	oldest := ib("1", "aloha", now.Add(-10*time.Minute))
	oldest2 := ib("2", "aloha", now.Add(-10*time.Minute))
	old := ib("4", "aloha", now)

	older2 := ib("3", "aloha2", now.Add(-5*time.Minute))
	older3 := ib("3", "aloha3", now.Add(-5*time.Minute))
	old3 := ib("keep", "aloha2", now)

	ctx := context.Background()
	schema, _ := hephv1.SchemeBuilder.Build()
	fakeClient := fake.NewClientBuilder().WithScheme(schema).WithObjects(&old, &oldest, &older2, &oldest2, &older3, &old3).Build()
	recorder := newRecorder(fakeClient)

	gc := &ImageBuildGC{
		HistoryLimit: 1,
		Client:       recorder.client,
		Namespaces:   []string{""},
	}

	err := gc.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}

	expected := []invocation{
		invokeList(""),
		invokeDelete(oldest),
		invokeDelete(oldest2),
		invokeDelete(older2),
		invokeDelete(older3),
		invokeDelete(old),
	}
	checkInvokes(t, expected, recorder.invokes)
}

func TestGCIBsListErr(t *testing.T) {
	errFailed := errors.New("failed")

	schema, _ := hephv1.SchemeBuilder.Build()
	fakeClient := fake.NewClientBuilder().WithScheme(schema).Build()
	iFakeClient := interceptor.NewClient(fakeClient, interceptor.Funcs{
		List: func(ctx context.Context, client client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errFailed
		},
	})

	gc := &ImageBuildGC{
		HistoryLimit: 1,
		Client:       iFakeClient,
		Namespaces:   []string{""},
	}
	err := gc.GC(context.Background())
	if errors.Is(err, errors.Join(errFailed)) {
		t.Fatal(err)
	}
}

func TestGCIBsDeleteErr(t *testing.T) {
	ib := ib("Success", "aloha", time.Now())

	errFailed := errors.New("failed")
	schema, _ := hephv1.SchemeBuilder.Build()
	fakeClient := fake.NewClientBuilder().WithScheme(schema).WithObjects(&ib).Build()
	iFakeClient := interceptor.NewClient(fakeClient, interceptor.Funcs{
		Delete: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return errFailed
		},
	})

	gc := &ImageBuildGC{
		HistoryLimit: 0,
		Client:       iFakeClient,
		Namespaces:   []string{""},
	}
	err := gc.GC(context.Background())
	if errors.Is(err, errors.Join(errFailed)) {
		t.Fatal(err)
	}
}

func TestGCNoIBs(t *testing.T) {
	schema, _ := hephv1.SchemeBuilder.Build()
	fakeClient := fake.NewClientBuilder().WithScheme(schema).Build()
	recorder := newRecorder(fakeClient)
	gc := &ImageBuildGC{
		HistoryLimit: 1,
		Client:       recorder.client,
		Namespaces:   []string{""},
	}
	err := gc.GC(context.Background())
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}

	expected := []invocation{
		invokeList(""),
	}

	checkInvokes(t, expected, recorder.invokes)
}

func checkInvokes(t *testing.T, expected []invocation, actual []invocation) {
	t.Helper()
	if e, a := len(expected), len(actual); e != a {
		t.Errorf("expected (%d) != actual (%d): %#v", e, a, actual)
	}

	for i := range expected {
		a := actual[i]
		e := expected[i]
		if a.operation != e.operation {
			t.Errorf("wrong operation %d: e=%v a=%v", i, e, a)
			continue
		}
		if a.NamespacedName != e.NamespacedName {
			t.Errorf("wrong NamespacedName %d: e=%v a=%v", i, e, a)
		}
		if !errors.Is(a.err, e.err) {
			t.Errorf("wrong error %d: e=%v a=%v", i, e, a)
		}
	}
}

type invocation struct {
	operation string
	types.NamespacedName
	runtime.Object
	err error
}

type recorder struct {
	client  client.WithWatch
	invokes []invocation
}

func newRecorder(c client.WithWatch) *recorder {
	r := &recorder{}
	r.client = interceptor.NewClient(c, interceptor.Funcs{List: r.List, Delete: r.Delete})
	return r
}

func (r *recorder) List(ctx context.Context, cl client.WithWatch, objList client.ObjectList, opts ...client.ListOption) error {
	err := cl.List(ctx, objList, opts...)
	options := (&client.ListOptions{}).ApplyOptions(opts)
	r.invokes = append(r.invokes, invocation{
		operation:      "list",
		NamespacedName: types.NamespacedName{Namespace: options.Namespace},
		err:            err,
	})
	return err
}

func (r *recorder) Delete(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
	err := cl.Delete(ctx, obj, opts...)
	r.invokes = append(r.invokes, invocation{
		operation:      "delete",
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()},
		Object:         obj.DeepCopyObject(),
		err:            err,
	})
	return err
}

func ib(name, ns string, creation time.Time) hephv1.ImageBuild {
	return hephv1.ImageBuild{
		Status: hephv1.ImageBuildStatus{
			Phase: hephv1.PhaseSucceeded,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(creation),
		},
	}
}

func invokeList(ns string) invocation {
	return invocation{
		operation:      "list",
		NamespacedName: types.NamespacedName{Namespace: ns},
	}
}

func invokeDelete(ib hephv1.ImageBuild) invocation {
	return invocation{
		operation:      "delete",
		NamespacedName: types.NamespacedName{Namespace: ib.Namespace, Name: ib.Name},
	}
}
