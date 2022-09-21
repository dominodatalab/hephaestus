package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/watch"
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
	owner      = "test-owner"
	namespace  = "test-namespace"
	testLabels = map[string]string{"owned-by": "testing"}
	testConfig = config.Buildkit{Namespace: namespace, PodLabels: testLabels, ServiceName: "buildkit", DaemonPort: 1234}

	validSts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "buildkit",
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: pointer.Int32(0),
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
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionTrue,
				},
				{
					Type:   corev1.PodInitialized,
					Status: corev1.ConditionTrue,
				},
				{
					Type:   corev1.ContainersReady,
					Status: corev1.ConditionTrue,
				},
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	validEndpointSlice = &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "buildkit-bgk87",
			Namespace: namespace,
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Conditions: discoveryv1.EndpointConditions{
					Ready:   pointer.Bool(true),
					Serving: pointer.Bool(true),
				},
				Hostname: pointer.String("buildkit-0"),
				TargetRef: &corev1.ObjectReference{
					Name:      "buildkit-0",
					Namespace: namespace,
				},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{
				Name: pointer.String("daemon"),
				Port: pointer.Int32(1234),
			},
		},
	}
)

func leasedPod() *corev1.Pod {
	leased := validPod.DeepCopy()
	leased.ObjectMeta.Annotations = map[string]string{
		leasedAtAnnotation:  time.Now().Format(time.RFC3339),
		leasedByAnnotation:  owner,
		managerIDAnnotation: string(newUUID()),
	}

	return leased
}

func assertLeasedPod(t *testing.T, action k8stesting.Action, ret *corev1.Pod) {
	t.Helper()

	patchAction := action.(k8stesting.PatchAction)

	assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType(), "unexpected patch type")

	pod := corev1.Pod{}
	patch := patchAction.GetPatch()
	if err := json.Unmarshal(patch, &pod); err != nil {
		assert.FailNowf(t, "unable to marshal patch into v1.Pod", "received invalid patch %s", patch)
	}

	assert.Contains(t, pod.Annotations, leasedByAnnotation)
	assert.Contains(t, pod.Annotations, managerIDAnnotation)
	assert.NotContains(t, pod.Annotations, expiryTimeAnnotation)

	ts, ok := pod.Annotations[leasedAtAnnotation]
	require.True(t, ok, "leased at annotation not found")

	leasedAt, err := time.Parse(time.RFC3339, ts)
	require.NoError(t, err, "invalid lease at annotation")

	assert.True(t, leasedAt.Before(time.Now()), "leased at is not in the past")

	ret.Annotations = pod.Annotations
}

// NOTE: this set of assertions is fine, but it's not great. we need a better way of asserting the patching. ideally, we
//
//	would make assertions against the API object after the event but client-go doesn't support SSA right now, which
//	means we have to override the "patch" action with a reactor.
func assertUnleasedPod(t *testing.T, action k8stesting.Action) {
	t.Helper()

	patchAction := action.(k8stesting.PatchAction)

	assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType(), "unexpected patch type")

	pp := corev1.Pod{}
	patch := patchAction.GetPatch()
	if err := json.Unmarshal(patch, &pp); err != nil {
		assert.FailNowf(t, "unable to marshal patch into v1.Pod", "received invalid patch %s", patch)
	}

	assert.NotContains(t, pp.Annotations, leasedAtAnnotation)
	assert.NotContains(t, pp.Annotations, leasedByAnnotation)
	assert.NotContains(t, pp.Annotations, managerIDAnnotation)

	ts, ok := pp.Annotations[expiryTimeAnnotation]
	require.True(t, ok, "expiry time annotation not found")

	expiry, err := time.Parse(time.RFC3339, ts)
	require.NoError(t, err, "invalid expiry time annotation")

	assert.True(t, expiry.After(time.Now().Add(5*time.Minute)), "expiry time is not in the future")
}

func TestPoolGet(t *testing.T) {
	t.Run("running_pod", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		p := validPod.DeepCopy()

		fakeClient := fake.NewSimpleClientset(p)
		fakeClient.PrependWatchReactor("endpointslices", func(k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()
				watcher.Add(validEndpointSlice)
			}()
			return true, watcher, nil
		})
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			assertLeasedPod(t, action, p)
			return true, p, nil
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		select {
		case res := <-leaseChannel:
			require.NoError(t, res.err, "could not acquire a buildkit endpoint")

			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			assert.Equal(t, expected, leaseAddr, "did not receive correct lease")
		case <-time.After(3 * time.Second):
			assert.Fail(t, "could not acquire a buildkit endpoint within 3s")
		}
	})

	t.Run("non_running_pod", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// non-running phase
		delivered := validPod.DeepCopy()
		delivered.Status.Phase = ""

		fakeClient := fake.NewSimpleClientset(delivered)
		fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()
				watcher.Add(validEndpointSlice)
			}()
			return true, watcher, nil
		})
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			assertLeasedPod(t, action, delivered)
			return true, delivered, nil
		})

		getExec := make(chan struct{})
		countDown := false
		reactionCount := 0
		fakeClient.PrependReactor("update", "statefulsets", func(k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			select {
			case <-getExec:
				countDown = true
			default:
			}

			// deliver running pod a few reconciliations after get is executed
			if countDown {
				if reactionCount == 2 {
					delivered = validPod.DeepCopy()
					fakeClient.PrependReactor("get", "pods", func(k8stesting.Action) (handled bool, ret runtime.Object, err error) {
						return true, delivered, nil
					})
					fakeClient.PrependReactor("list", "pods", func(k8stesting.Action) (handled bool, ret runtime.Object, err error) {
						return true, &corev1.PodList{Items: []corev1.Pod{*delivered}}, nil
					})
				}
				reactionCount++
			}

			return true, nil, nil
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			getExec <- struct{}{}

			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		select {
		case res := <-leaseChannel:
			require.NoError(t, res.err, "could not acquire a buildkit endpoint")
			require.Equal(t, delivered.Status.Phase, corev1.PodRunning, "non-running pod returned")

			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			assert.Equal(t, expected, leaseAddr, "did not receive correct lease")
		case <-time.After(3 * time.Second):
			assert.Fail(t, "could not acquire a buildkit endpoint within 3s")
		}
	})

	t.Run("lease_failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeClient := fake.NewSimpleClientset(validPod)
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("expected failure")
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		select {
		case res := <-leaseChannel:
			assert.Empty(t, res.res.(string), "expected an empty lease address")
			assert.EqualError(t, res.err, "cannot update pod metadata: expected failure")
		case <-time.After(3 * time.Second):
			assert.Fail(t, "could not acquire a buildkit endpoint within 3s")
		}
	})

	t.Run("endpoints_failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeClient := fake.NewSimpleClientset(validPod)
		fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()

				eps := validEndpointSlice.DeepCopy()
				eps.Endpoints = nil
				watcher.Add(eps)
			}()
			return true, watcher, nil
		})

		reactionCount := 0
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			p := validPod.DeepCopy()

			switch reactionCount {
			case 0:
				assertLeasedPod(t, action, p)
			case 1:
				assertUnleasedPod(t, action)
			default:
				assert.FailNow(t, "pod patched more than twice")
			}
			reactionCount++

			return true, p, nil
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		select {
		case res := <-leaseChannel:
			assert.Empty(t, res.res.(string), "expected an empty lease address")
			assert.EqualError(t, res.err, "failed to extract hostname after 180 seconds")
		case <-time.After(3 * time.Second):
			assert.Fail(t, "could not acquire a buildkit endpoint within 3s")
		}
	})

	t.Run("endpoints_lag", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeClient := fake.NewSimpleClientset(validPod)
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			p := validPod.DeepCopy()
			assertLeasedPod(t, action, p)
			return true, p, nil
		})

		fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()

				eps := validEndpointSlice.DeepCopy()
				eps.Endpoints = nil
				watcher.Add(eps)

				eps = validEndpointSlice.DeepCopy()
				eps.Endpoints[0].Conditions.Ready = pointer.Bool(false)
				watcher.Add(eps)

				eps = validEndpointSlice.DeepCopy()
				eps.Endpoints[0].Hostname = nil
				watcher.Add(eps)

				watcher.Add(validEndpointSlice)
			}()

			return true, watcher, nil
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		select {
		case res := <-leaseChannel:
			require.NoError(t, res.err, "could not acquire a buildkit endpoint")

			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			assert.Equal(t, expected, leaseAddr, "did not receive correct lease")
		case <-time.After(3 * time.Second):
			assert.Fail(t, "could not acquire a buildkit endpoint within 3s")
		}
	})

	t.Run("scale_up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeClient := fake.NewSimpleClientset(validSts)
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, leasedPod(), nil
		})

		stsUpdateChan := make(chan struct{})
		errorChan := make(chan error)

		go func() {
			<-stsUpdateChan

			if _, err := fakeClient.CoreV1().Pods(namespace).Create(ctx, validPod, metav1.CreateOptions{}); err != nil {
				errorChan <- err
				return
			}

			fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
				watcher := watch.NewFake()
				go func() {
					defer watcher.Stop()
					watcher.Add(validEndpointSlice)
				}()
				return true, watcher, nil
			})
		}()

		fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			select {
			case stsUpdateChan <- struct{}{}:
			default:
			}

			return true, nil, nil
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		select {
		case res := <-leaseChannel:
			if res.err != nil {
				t.Errorf("could not acquire a buildkit endpoint: %s", res.err.Error())
			} else {
				leaseAddr := res.res.(string)
				expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
				if leaseAddr != expected {
					t.Errorf("did not receive correct lease: %s expected, %s actual", expected, leaseAddr)
				}
			}
		case e := <-errorChan:
			if e != nil {
				t.Errorf("Received error attempting to create test setup: %s", e.Error())
			}
		case <-time.After(3 * time.Second):
			t.Error("Could not acquire a buildkit endpoint within 3s")
		}
	})
}

func TestPoolGetFailedScaleUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeClient := fake.NewSimpleClientset(validSts, validPod)

	fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		watcher := watch.NewFake()
		go func() {
			defer watcher.Stop()
			watcher.Add(validEndpointSlice)
		}()
		return true, watcher, nil
	})

	leased := false
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		if leased {
			return true, validPod, nil
		}

		leased = !leased
		return true, leasedPod(), nil
	})

	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.New("failed scale up")
	})

	wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
	defer wp.Close()

	addr, err := wp.Get(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}

	leaseChannel := make(chan result)
	go func() {
		addr, err := wp.Get(ctx, owner)
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
				t.Errorf("did not received correct lease: %s expected, %s actual", expected, leaseAddr)
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("Could not acquire a buildkit endpoint within 3s")
	}
}

func TestPoolGetAndClose(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(validSts)
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, validPod, nil
	})
	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, nil
	})

	wp := NewPool(context.Background(), fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))

	done := make(chan struct{})
	go func() {
		addr, err := wp.Get(context.Background(), owner)
		assert.Empty(t, addr, "acquired lease even though pool was closed")
		assert.Equal(t, ErrNoUnleasedPods, err, "expected no unleased pods error")

		done <- struct{}{}
	}()
	time.Sleep(1 * time.Millisecond)

	wp.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		assert.Fail(t, "worker pool was not closed within 3s")
	}
}

func TestPoolGetAndCancel(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(validSts)
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, validPod, nil
	})
	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, nil
	})

	wp := NewPool(context.Background(), fakeClient, testConfig, SyncWaitTime(250*time.Millisecond))
	defer wp.Close()

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		addr, err := wp.Get(ctx, owner)
		assert.Empty(t, addr, "acquired lease even though pool was closed")
		assert.Equal(t, context.Canceled, err)

		done <- struct{}{}
	}()
	time.Sleep(1 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		assert.Fail(t, "worker pool was not closed within 3s")
	}
}

func TestPoolRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("success", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset(leasedPod())
		fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			assertUnleasedPod(t, action)
			return true, nil, nil
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()

		assert.NoError(t, wp.Release(ctx, "tcp://buildkit-0.buildkit.default:1234"), "expected release to succeed")
	})

	t.Run("invalid_address", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset(leasedPod())

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()

		invalidAddrs := []string{
			"buildkit-0.buildkit.default:1234",
			"buildkit-0.buildkit.default",
			"buildkit-0",
			"tcp://",
		}

		for _, addr := range invalidAddrs {
			assert.EqualErrorf(t,
				wp.Release(ctx, addr),
				"invalid address: must be an absolute URI including scheme",
				"expected %s to produce an uri parse err", addr,
			)
		}
	})

	t.Run("missing_pod", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset()

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()

		assert.EqualError(t,
			wp.Release(ctx, "tcp://buildkit-0.buildkit.default:1234"),
			`addr "tcp://buildkit-0.buildkit.default:1234" is not allocated: pods "buildkit-0" not found`,
		)
	})

	t.Run("patch_fail", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset(leasedPod())
		fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("test failure")
		})

		wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()

		assert.EqualError(t, wp.Release(ctx, "tcp://buildkit-0.buildkit.default:1234"), "cannot update pod metadata: test failure")
	})
}

func TestPoolPodReconciliation(t *testing.T) {
	tests := []struct {
		name     string
		objects  func() []runtime.Object
		requests int
		expected int32
	}{
		{
			name: "unmanaged",
			objects: func() []runtime.Object {
				p := leasedPod()
				p.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

				return []runtime.Object{p}
			},
			expected: 0,
		},
		{
			name: "leased",
			objects: func() []runtime.Object {
				return []runtime.Object{leasedPod()}
			},
			expected: 1,
		},
		{
			name: "unleased_new",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.CreationTimestamp = metav1.Time{Time: time.Now()}

				return []runtime.Object{p}
			},
			expected: 1,
		},
		{
			name: "unleased_old",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.CreationTimestamp = metav1.Time{Time: time.Now().Add(-20 * time.Minute)}

				return []runtime.Object{p}
			},
			expected: 0,
		},
		{
			name: "pending",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.Status.Phase = corev1.PodPending

				return []runtime.Object{p}
			},
			expected: 0,
		},
		{
			name: "phase_unknown",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.Status.Phase = ""

				return []runtime.Object{p}
			},
			expected: 1,
			requests: 1,
		},
		{
			name: "expiry_upcoming",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.CreationTimestamp = metav1.Time{Time: time.Now()}
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(20 * time.Minute).Format(time.RFC3339),
				}

				return []runtime.Object{p}
			},
			expected: 1,
		},
		{
			name: "expiry_past",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.CreationTimestamp = metav1.Time{Time: time.Now()}
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
				}

				return []runtime.Object{p}
			},
			expected: 0,
		},
		{
			name: "expiry_invalid",
			objects: func() []runtime.Object {
				p := validPod.DeepCopy()
				p.CreationTimestamp = metav1.Time{Time: time.Now()}
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: "garbage",
				}

				return []runtime.Object{p}
			},
			expected: 0,
		},
		{
			name: "no_pods",
			objects: func() []runtime.Object {
				return nil
			},
			expected: 0,
		},
		{
			name: "combination_trim",
			objects: func() []runtime.Object {
				leased := leasedPod()
				leased.Name = "buildkit-0"

				unexpired := validPod.DeepCopy()
				unexpired.Name = "buildkit-1"
				unexpired.CreationTimestamp = metav1.Time{Time: time.Now()}
				unexpired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(20 * time.Minute).Format(time.RFC3339),
				}

				fresh := validPod.DeepCopy()
				fresh.Name = "buildkit-2"
				fresh.CreationTimestamp = metav1.Time{Time: time.Now()}

				unmanaged := leasedPod()
				unmanaged.Name = "buildkit-3"
				unmanaged.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

				expired := validPod.DeepCopy()
				expired.Name = "buildkit-4"
				expired.CreationTimestamp = metav1.Time{Time: time.Now()}
				expired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
				}

				pending := validPod.DeepCopy()
				pending.Name = "buildkit-5"
				pending.Status.Phase = corev1.PodPending

				return []runtime.Object{leased, unexpired, fresh, unmanaged, expired, pending}
			},
			expected: 3,
		},
		{
			name: "combination_halt",
			objects: func() []runtime.Object {
				unmanaged := leasedPod()
				unmanaged.Name = "buildkit-0"
				unmanaged.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

				expired := validPod.DeepCopy()
				expired.Name = "buildkit-1"
				expired.CreationTimestamp = metav1.Time{Time: time.Now()}
				expired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
				}

				leased := leasedPod()
				leased.Name = "buildkit-2"

				fresh := validPod.DeepCopy()
				fresh.Name = "buildkit-3"
				fresh.CreationTimestamp = metav1.Time{Time: time.Now()}

				pending := validPod.DeepCopy()
				pending.Name = "buildkit-4"
				pending.Status.Phase = corev1.PodPending

				return []runtime.Object{unmanaged, expired, leased, fresh, pending}
			},
			expected: 4,
		},
		{
			name: "combination_stand",
			objects: func() []runtime.Object {
				unexpired := validPod.DeepCopy()
				unexpired.Name = "buildkit-0"
				unexpired.CreationTimestamp = metav1.Time{Time: time.Now()}
				unexpired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(20 * time.Minute).Format(time.RFC3339),
				}

				leased := leasedPod()
				leased.Name = "buildkit-1"

				fresh := validPod.DeepCopy()
				fresh.Name = "buildkit-2"
				fresh.CreationTimestamp = metav1.Time{Time: time.Now()}

				return []runtime.Object{unexpired, leased, fresh}
			},
			expected: 3,
		},
		{
			name: "combination_flush",
			objects: func() []runtime.Object {
				expired := validPod.DeepCopy()
				expired.Name = "buildkit-0"
				expired.CreationTimestamp = metav1.Time{Time: time.Now()}
				expired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
				}

				pending := validPod.DeepCopy()
				pending.Name = "buildkit-1"
				pending.Status.Phase = corev1.PodPending

				old := validPod.DeepCopy()
				old.Name = "buildkit-2"
				old.CreationTimestamp = metav1.Time{Time: time.Now().Add(-20 * time.Minute)}

				return []runtime.Object{expired, pending, old}
			},
			expected: 0,
		},
		{
			name: "combination_embedded_failure_no_growth",
			objects: func() []runtime.Object {
				p0 := validPod.DeepCopy()
				p0.Name = "buildkit-0"
				p0.Status.Phase = corev1.PodPending

				p1 := validPod.DeepCopy()
				p1.Name = "buildkit-1"
				p1.Status.Phase = corev1.PodReasonUnschedulable

				p2 := validPod.DeepCopy()
				p2.Name = "buildkit-2"
				p2.Status.Phase = corev1.PodFailed

				p3 := validPod.DeepCopy()
				p3.Name = "buildkit-3"
				p3.Status.Phase = corev1.PodPending

				return []runtime.Object{p0, p1, p2, p3}
			},
			requests: 4,
			expected: 4,
		},
		{
			name: "combination_embedded_failure_trim",
			objects: func() []runtime.Object {
				p0 := validPod.DeepCopy()
				p0.Name = "buildkit-0"
				p0.Status.Phase = corev1.PodPending

				p1 := validPod.DeepCopy()
				p1.Name = "buildkit-1"
				p1.Status.Phase = corev1.PodReasonUnschedulable

				p2 := validPod.DeepCopy()
				p2.Name = "buildkit-2"
				p2.Status.Phase = corev1.PodFailed

				p3 := validPod.DeepCopy()
				p3.Name = "buildkit-3"
				p3.Status.Phase = corev1.PodPending

				return []runtime.Object{p0, p1, p2, p3}
			},
			requests: 1,
			expected: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			replicasCh := make(chan int32)
			defer close(replicasCh)

			fakeClient := fake.NewSimpleClientset(tc.objects()...)
			fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
				scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
				replicasCh <- scale.Spec.Replicas

				cancel()
				return true, nil, nil
			})

			wp := NewPool(ctx, fakeClient, testConfig, SyncWaitTime(250*time.Millisecond), MaxIdleTime(10*time.Minute))
			for i := 0; i < tc.requests; i++ {
				wp.requests.Enqueue(&PodRequest{result: make(chan PodRequestResult, 1)})
			}
			defer wp.Close()

			select {
			case actual := <-replicasCh:
				if tc.expected != actual {
					t.Errorf("expected statefulset update with %d replicas, got %d", tc.expected, actual)
				}
			case <-time.After(3 * time.Second):
				t.Error("worker pool update not received within 3s")
			}
		})
	}
}
