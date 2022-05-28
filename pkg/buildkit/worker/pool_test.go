package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/pointer"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

func init() {
	newUUID = func() types.UID { return "manager-id" }
}

type result struct {
	res any
	err error
}

var (
	namespace  = "test-namespace"
	testLabels = map[string]string{"owned-by": "testing"}

	validSts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "buildkit",
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: pointer.Int32Ptr(0),
			Selector: &metav1.LabelSelector{
				MatchLabels: testLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: testLabels,
				},
				Spec: corev1.PodSpec{},
			},
		},
	}

	validPod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "buildkit-0",
			Namespace: namespace,
			Labels:    testLabels,
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	validEndpoints = &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "buildkit",
			Namespace: namespace,
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP:       "1.2.3.4",
						Hostname: "buildkit-0",
						TargetRef: &corev1.ObjectReference{
							Namespace: namespace,
							Name:      "buildkit-0",
						},
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "buildkit",
						Port:     1234,
						Protocol: "tcp",
					},
				},
			},
		},
	}
)

func TestPoolGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeClient := fake.NewSimpleClientset(validSts)
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		p := validPod.DeepCopy()
		p.ObjectMeta.Annotations = map[string]string{
			leasedAnnotation:    "true",
			managerIDAnnotation: string(newUUID()),
		}
		return true, p, nil
	})

	stsUpdateChan := make(chan struct{})
	errorChan := make(chan error)

	go func() {
		<-stsUpdateChan

		if _, err := fakeClient.CoreV1().Pods(namespace).Create(ctx, validPod, metav1.CreateOptions{}); err != nil {
			errorChan <- err
			return
		}

		if _, err := fakeClient.CoreV1().Endpoints(namespace).Create(ctx, validEndpoints, metav1.CreateOptions{}); err != nil {
			errorChan <- err
			return
		}
	}()

	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		select {
		case stsUpdateChan <- struct{}{}:
		default:
		}

		return true, nil, nil
	})

	cfg := config.Buildkit{Namespace: namespace, PodLabels: testLabels, ServiceName: "buildkit", DaemonPort: 1234}
	wp := NewPool(ctx, fakeClient, cfg, SyncWaitTime(250*time.Millisecond))
	defer wp.Close()

	leaseChannel := make(chan result)

	go func() {
		addr, err := wp.Get(ctx)
		leaseChannel <- result{addr, err}
	}()

	select {
	case res := <-leaseChannel:
		if res.err != nil {
			t.Errorf("Could not acquire a buildkit endpoint: %s", res.err.Error())
		} else {
			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			if leaseAddr != expected {
				t.Errorf("Did not received correct lease: %s expected, %s actual", expected, leaseAddr)
			}
		}
	case e := <-errorChan:
		if e != nil {
			t.Errorf("Received error attempting to create test setup: %s", e.Error())
		}
	case <-time.After(3 * time.Second):
		t.Error("Could not acquire a buildkit endpoint within 3s")
	}
}

func TestPoolGetFailedScaleUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeClient := fake.NewSimpleClientset(validSts, validPod, validEndpoints)

	leased := false
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		if leased {
			return true, validPod, nil
		}

		leased = !leased
		p := validPod.DeepCopy()
		p.ObjectMeta.Annotations = map[string]string{
			leasedAnnotation:    "true",
			managerIDAnnotation: string(newUUID()),
		}
		return true, p, nil
	})

	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.New("failed scale up")
	})

	cfg := config.Buildkit{Namespace: namespace, PodLabels: testLabels, ServiceName: "buildkit", DaemonPort: 1234}
	wp := NewPool(ctx, fakeClient, cfg, SyncWaitTime(250*time.Millisecond))
	defer wp.Close()

	addr, err := wp.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	leaseChannel := make(chan result)
	go func() {
		addr, err := wp.Get(ctx)
		leaseChannel <- result{addr, err}
	}()

	if err := wp.Release(ctx, addr); err != nil {
		t.Fatal(err)
	}

	select {
	case res := <-leaseChannel:
		if res.err != nil {
			t.Errorf("Could not acquire a buildkit endpoint: %s", res.err.Error())
		} else {
			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			if leaseAddr != expected {
				t.Errorf("Did not received correct lease: %s expected, %s actual", expected, leaseAddr)
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("Could not acquire a buildkit endpoint within 3s")
	}
}

func TestPoolGetCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeClient := fake.NewSimpleClientset(validSts)
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, validPod, nil
	})
	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, nil
	})

	cfg := config.Buildkit{Namespace: namespace, PodLabels: testLabels, ServiceName: "buildkit", DaemonPort: 1234}
	wp := NewPool(ctx, fakeClient, cfg, SyncWaitTime(250*time.Millisecond))

	leaseChannel := make(chan result)

	go func() {
		addr, err := wp.Get(context.Background())
		leaseChannel <- result{addr, err}
	}()

	wp.Close()

	select {
	case res := <-leaseChannel:
		if res.res.(string) != "" || res.err == nil {
			t.Errorf("Acquired lease even though pool was closed: %v", res)
		}

		if res.err != ErrNoUnleasedPods {
			t.Errorf("Expected no unleased pods error, received: %v", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Worker pool was not closed within 3s")
	}
}
