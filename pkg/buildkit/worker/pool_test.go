package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"
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
)

func TestPoolGet(t *testing.T) {
	t.Run("running_pod", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		p := validPod()

		fakeClient := fake.NewSimpleClientset(p)
		fakeClient.PrependWatchReactor("endpointslices", func(k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()
				watcher.Add(validEndpointSlice(p))
			}()
			return true, watcher, nil
		})
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			assertLeasedPod(t, action, p)
			return true, p, nil
		})

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		wp.Start(ctx)

		select {
		case res := <-leaseChannel:
			require.NoError(t, res.err, "could not acquire a buildkit endpoint")

			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			assert.Equal(t, expected, leaseAddr, "did not receive correct lease")
		}
	})

	t.Run("non_running_pod", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// non-running phase
		delivered := validPod()
		delivered.Status.Phase = ""

		fakeClient := fake.NewSimpleClientset(delivered)
		fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()
				watcher.Add(validEndpointSlice(delivered))
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
					delivered = validPod()
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

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			getExec <- struct{}{}

			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		wp.Start(ctx)

		select {
		case res := <-leaseChannel:
			require.NoError(t, res.err, "could not acquire a buildkit endpoint")
			require.Equal(t, delivered.Status.Phase, corev1.PodRunning, "non-running pod returned")

			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			assert.Equal(t, expected, leaseAddr, "did not receive correct lease")
		}
	})

	t.Run("lease_failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeClient := fake.NewSimpleClientset(validPod())
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("expected failure")
		})

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		wp.Start(ctx)

		select {
		case res := <-leaseChannel:
			assert.Empty(t, res.res.(string), "expected an empty lease address")
			assert.EqualError(t, res.err, "cannot update pod metadata: expected failure")
		}
	})

	t.Run("endpoints_failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeClient := fake.NewSimpleClientset(validPod())
		fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				defer watcher.Stop()

				eps := validEndpointSlice()
				watcher.Add(eps)
			}()
			return true, watcher, nil
		})

		reactionCount := 0
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			p := validPod()

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

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		wp.Start(ctx)

		select {
		case res := <-leaseChannel:
			assert.Empty(t, res.res.(string), "expected an empty lease address")
			assert.EqualError(t, res.err, "failed to extract hostname after 180 seconds")
		}
	})

	t.Run("endpoints_lag", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		p := validPod()
		fakeClient := fake.NewSimpleClientset(p)
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			assertLeasedPod(t, action, p)
			return true, p, nil
		})

		fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
			watcher := watch.NewFake()
			go func() {
				eps := validEndpointSlice()
				watcher.Add(eps)

				eps = validEndpointSlice(p)
				eps.Endpoints[0].Conditions.Ready = pointer.Bool(false)
				watcher.Add(eps)

				eps = validEndpointSlice(p)
				eps.Endpoints[0].Hostname = nil
				watcher.Add(eps)

				watcher.Add(validEndpointSlice(p))
			}()

			return true, watcher, nil
		})

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		wp.Start(ctx)

		select {
		case res := <-leaseChannel:
			require.NoError(t, res.err, "could not acquire a buildkit endpoint")

			leaseAddr := res.res.(string)
			expected := "tcp://buildkit-0.buildkit.test-namespace:1234"
			assert.Equal(t, expected, leaseAddr, "did not receive correct lease")
		}
	})

	t.Run("scale_up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		p := leasedPod()
		fakeClient := fake.NewSimpleClientset(validSts())
		fakeClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, p, nil
		})

		stsUpdateChan := make(chan struct{})
		errorChan := make(chan error)

		go func() {
			<-stsUpdateChan

			if _, err := fakeClient.CoreV1().Pods(namespace).Create(ctx, validPod(), metav1.CreateOptions{}); err != nil {
				errorChan <- err
				return
			}

			fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
				watcher := watch.NewFake()
				go func() {
					defer watcher.Stop()
					watcher.Add(validEndpointSlice(p))
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

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
		defer wp.Close()

		leaseChannel := make(chan result)
		go func() {
			addr, err := wp.Get(ctx, owner)
			leaseChannel <- result{addr, err}
		}()

		wp.Start(ctx)

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
		}
	})
}

func TestPoolGetFailedScaleUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := validPod()
	fakeClient := fake.NewSimpleClientset(validSts(), p)

	fakeClient.PrependWatchReactor("endpointslices", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		watcher := watch.NewFake()
		go func() {
			defer watcher.Stop()
			watcher.Add(validEndpointSlice(p))
		}()
		return true, watcher, nil
	})

	leased := false
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		if leased {
			return true, validPod(), nil
		}

		leased = !leased
		return true, leasedPod(), nil
	})

	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.New("failed scale up")
	})

	wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
	defer wp.Close()
	wp.Start(ctx)

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
	}
}

func TestPoolGetAndClose(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(validSts())
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, validPod(), nil
	})
	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, nil
	})

	wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
	wp.Start(context.Background())

	errCh := make(chan error)
	go func() {
		addr, err := wp.Get(context.Background(), owner)
		if addr != "" {
			err = fmt.Errorf("acquired lease even though pool was closed")
		} else if err == ErrNoUnleasedPods {
			err = nil
		}
		errCh <- err
	}()

	wp.Close()

	err := <-errCh
	if err != nil {
		t.Error(err)
	}
}

func TestPoolGetAndCancel(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(validSts())
	fakeClient.PrependReactor("patch", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, validPod(), nil
	})
	fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, nil
	})

	wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond))
	defer wp.Close()
	wp.Start(context.Background())

	errCh := make(chan error)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		addr, err := wp.Get(ctx, owner)
		if addr != "" {
			err = fmt.Errorf("acquired lease even though pool was closed")
		} else if err == context.Canceled {
			err = nil
		}
		errCh <- err
	}()

	cancel()

	err := <-errCh
	if err != nil {
		t.Error(err)
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

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()
		wp.Start(ctx)

		assert.NoError(t, wp.Release(ctx, "tcp://buildkit-0.buildkit.default:1234"), "expected release to succeed")
	})

	t.Run("invalid_address", func(t *testing.T) {
		fakeClient := fake.NewSimpleClientset(leasedPod())

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()
		wp.Start(ctx)

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

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()
		wp.Start(ctx)

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

		wp := NewPool(fakeClient, testConfig, SyncWaitTime(50*time.Millisecond), MaxIdleTime(10*time.Minute))
		defer wp.Close()
		wp.Start(ctx)

		assert.EqualError(t, wp.Release(ctx, "tcp://buildkit-0.buildkit.default:1234"), "cannot update pod metadata: test failure")
	})
}

func TestPoolPodReconciliation(t *testing.T) {
	tests := []struct {
		name             string
		objects          func() []runtime.Object
		buildRequests    int
		expectedReplicas int32
	}{
		{
			name: "no_pods",
			objects: func() []runtime.Object {
				return nil
			},
			expectedReplicas: 0,
		},
		{
			name: "no_pods_with_requests",
			objects: func() []runtime.Object {
				return nil
			},
			buildRequests:    1,
			expectedReplicas: 1,
		},
		{
			name: "unmanaged",
			objects: func() []runtime.Object {
				return []runtime.Object{unmanagedPod()}
			},
			expectedReplicas: 0,
		},
		{
			name: "unmanaged_with_requests",
			objects: func() []runtime.Object {
				return []runtime.Object{unmanagedPod()}
			},
			buildRequests:    1,
			expectedReplicas: 0,
		},
		{
			name: "leased",
			objects: func() []runtime.Object {
				p := leasedPod()
				return []runtime.Object{p}
			},
			expectedReplicas: 1,
		},
		{
			name: "leased_with_requests",
			objects: func() []runtime.Object {
				p := leasedPod()
				return []runtime.Object{p}
			},
			buildRequests:    1,
			expectedReplicas: 2,
		},
		{
			name: "pending",
			objects: func() []runtime.Object {
				p := pendingPod()
				return []runtime.Object{p}
			},
			expectedReplicas: 1,
		},
		{
			name: "pending_with_requests",
			objects: func() []runtime.Object {
				p := pendingPod()
				return []runtime.Object{p}
			},
			buildRequests:    1,
			expectedReplicas: 1,
		},
		{
			name: "pending_old",
			objects: func() []runtime.Object {
				p := pendingPod()
				p.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
				return []runtime.Object{p}
			},
			expectedReplicas: 0,
		},
		{
			name: "pending_old_with_requests",
			objects: func() []runtime.Object {
				p := pendingPod()
				p.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
				return []runtime.Object{p}
			},
			buildRequests:    1,
			expectedReplicas: 0,
		},
		{
			name: "starting",
			objects: func() []runtime.Object {
				p := validPod()
				p.Status.Conditions[2].Status = corev1.ConditionFalse
				return []runtime.Object{p}
			},
			expectedReplicas: 1,
		},
		{
			name: "starting_old",
			objects: func() []runtime.Object {
				p := validPod()
				p.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
				p.Status.Conditions[2].Status = corev1.ConditionFalse
				return []runtime.Object{p}
			},
			expectedReplicas: 0,
		},
		{
			name: "starting_with_requests",
			objects: func() []runtime.Object {
				p := validPod()
				p.Status.Conditions[2].Status = corev1.ConditionFalse
				return []runtime.Object{p}
			},
			buildRequests:    1,
			expectedReplicas: 1,
		},
		{
			name: "operational",
			objects: func() []runtime.Object {
				p := validPod()
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				}
				return []runtime.Object{p}
			},
			expectedReplicas: 1,
		},
		{
			name: "operational_with_requests",
			objects: func() []runtime.Object {
				p := validPod()
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				}
				return []runtime.Object{p}
			},
			buildRequests:    1,
			expectedReplicas: 1,
		},
		{
			name: "operational_past_expiry",
			objects: func() []runtime.Object {
				p := validPod()
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				}
				return []runtime.Object{p}
			},
			expectedReplicas: 0,
		},
		{
			name: "operational_invalid_expiry",
			objects: func() []runtime.Object {
				p := validPod()
				p.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: "garbage",
				}
				return []runtime.Object{p}
			},
			expectedReplicas: 0,
		},
		{
			name: "unknown",
			objects: func() []runtime.Object {
				p := validPod()
				p.Status.Phase = ""
				return []runtime.Object{p}
			},
			expectedReplicas: 0,
		},
		{
			name: "unknown_with_requests",
			objects: func() []runtime.Object {
				p := validPod()
				p.Status.Phase = ""
				return []runtime.Object{p}
			},
			buildRequests:    1,
			expectedReplicas: 0,
		},
		{
			name: "combo_preserve_all",
			objects: func() []runtime.Object {
				unexpired := validPod()
				unexpired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				}

				leased := leasedPod()
				leased.Name = "buildkit-1"

				fresh := validPod()
				fresh.Name = "buildkit-2"

				return []runtime.Object{unexpired, leased, fresh}
			},
			expectedReplicas: 3,
		},
		{
			name: "combo_remove_all",
			objects: func() []runtime.Object {
				expired := validPod()
				expired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				}

				oldPending := pendingPod()
				oldPending.Name = "buildkit-1"
				oldPending.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))

				oldUnused := validPod()
				oldUnused.Name = "buildkit-2"
				oldUnused.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))

				return []runtime.Object{expired, oldPending, oldUnused}
			},
			expectedReplicas: 0,
		},
		{
			name: "combo_wait_for_pending",
			objects: func() []runtime.Object {
				leased0 := leasedPod()

				pending1 := pendingPod()
				pending1.Name = "buildkit-1"

				pending2 := pendingPod()
				pending2.Name = "buildkit-2"

				return []runtime.Object{leased0, pending1, pending2}
			},
			expectedReplicas: 3,
		},
		{
			name: "combo_wait_for_pending_with_requests",
			objects: func() []runtime.Object {
				leased0 := leasedPod()

				pending1 := pendingPod()
				pending1.Name = "buildkit-1"

				pending2 := pendingPod()
				pending2.Name = "buildkit-2"

				return []runtime.Object{leased0, pending1, pending2}
			},
			buildRequests:    2,
			expectedReplicas: 3,
		},
		{
			name: "combo_grow_pending_with_requests",
			objects: func() []runtime.Object {
				leased0 := leasedPod()

				pending1 := pendingPod()
				pending1.Name = "buildkit-1"

				pending2 := pendingPod()
				pending2.Name = "buildkit-2"

				return []runtime.Object{leased0, pending1, pending2}
			},
			buildRequests:    3,
			expectedReplicas: 4,
		},
		{
			name: "combo_trim_tail",
			objects: func() []runtime.Object {
				leased := leasedPod()
				leased.Name = "buildkit-0"

				unexpired := validPod()
				unexpired.Name = "buildkit-1"
				unexpired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				}

				fresh := validPod()
				fresh.Name = "buildkit-2"

				unmanaged := leasedPod()
				unmanaged.Name = "buildkit-3"
				unmanaged.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

				expired := validPod()
				expired.Name = "buildkit-4"
				expired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
				}

				expiredPending := validPod()
				expiredPending.Name = "buildkit-5"
				expiredPending.Status.Phase = corev1.PodPending
				expiredPending.CreationTimestamp = metav1.NewTime(time.Now().Add(-20 * time.Minute))

				return []runtime.Object{leased, unexpired, fresh, unmanaged, expired, expiredPending}
			},
			expectedReplicas: 3,
		},
		{
			name: "combo_trim_tail_with_equal_requests",
			objects: func() []runtime.Object {
				leased := leasedPod()
				leased.Name = "buildkit-0"

				unexpired := validPod()
				unexpired.Name = "buildkit-1"
				unexpired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(20 * time.Minute).Format(time.RFC3339),
				}

				fresh := validPod()
				fresh.Name = "buildkit-2"

				unmanaged := leasedPod()
				unmanaged.Name = "buildkit-3"
				unmanaged.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

				expired := validPod()
				expired.Name = "buildkit-4"
				expired.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
				}

				expiredPending := validPod()
				expiredPending.Name = "buildkit-5"
				expiredPending.Status.Phase = corev1.PodPending
				expiredPending.CreationTimestamp = metav1.NewTime(time.Now().Add(-20 * time.Minute))

				return []runtime.Object{leased, unexpired, fresh, unmanaged, expired, expiredPending}
			},
			buildRequests:    2,
			expectedReplicas: 3,
		},
		{
			name: "combo_trim_tail_with_extended_requests",
			objects: func() []runtime.Object {
				leased0 := leasedPod()
				leased0.Name = "buildkit-0"

				unexpired1 := validPod()
				unexpired1.Name = "buildkit-1"
				unexpired1.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(10 * time.Minute).Format(time.RFC3339),
				}

				fresh2 := validPod()
				fresh2.Name = "buildkit-2"

				unmanaged3 := leasedPod()
				unmanaged3.Name = "buildkit-3"
				unmanaged3.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

				expired4 := validPod()
				expired4.Name = "buildkit-4"
				expired4.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				}

				expiredPending5 := pendingPod()
				expiredPending5.Name = "buildkit-5"
				expiredPending5.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))

				return []runtime.Object{leased0, unexpired1, fresh2, unmanaged3, expired4, expiredPending5}
			},
			buildRequests:    3,
			expectedReplicas: 5,
		},
		{
			name: "combo_embedded_failure_growth",
			objects: func() []runtime.Object {
				p0 := pendingPod()
				p0.Name = "buildkit-0"

				p1 := validPod()
				p1.Name = "buildkit-1"
				p1.Status.Phase = corev1.PodReasonUnschedulable

				p2 := validPod()
				p2.Name = "buildkit-2"
				p2.Status.Phase = corev1.PodFailed

				p3 := pendingPod()
				p3.Name = "buildkit-3"

				return []runtime.Object{p0, p1, p2, p3}
			},
			buildRequests:    4,
			expectedReplicas: 4,
		},
		{
			name: "combo_embedded_failure_trim",
			objects: func() []runtime.Object {
				p0 := pendingPod()

				p1 := validPod()
				p1.Name = "buildkit-1"
				p1.Status.Phase = corev1.PodReasonUnschedulable

				p2 := validPod()
				p2.Name = "buildkit-2"
				p2.Status.Phase = corev1.PodFailed

				p3 := pendingPod()
				p3.Name = "buildkit-3"
				p3.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))

				return []runtime.Object{p0, p1, p2, p3}
			},
			buildRequests:    1,
			expectedReplicas: 1,
		},
		{
			name: "combo_trim_expired",
			objects: func() []runtime.Object {
				leased0 := leasedPod()
				leased0.Name = "buildkit-0"

				expired1 := validPod()
				expired1.Name = "buildkit-1"
				expired1.ObjectMeta.Annotations = map[string]string{
					expiryTimeAnnotation: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
				}
				return []runtime.Object{leased0, expired1}
			},
			buildRequests:    0,
			expectedReplicas: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			updateCh := make(chan int32, 1)

			objects := tc.objects()
			fakeClient := fake.NewSimpleClientset(objects...)
			fakeClient.PrependWatchReactor("endpointslices", func(a k8stesting.Action) (handled bool, ret watch.Interface, err error) {
				watcher := watch.NewFake()
				if objects != nil {
					go func() {
						watcher.Add(validEndpointSlice(objects...))
						t.Log("added endpoints")
					}()
				}
				return true, watcher, nil
			})
			fakeClient.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
				updateCh <- scale.Spec.Replicas

				return true, nil, nil
			})

			wp := NewPool(fakeClient, testConfig, SyncWaitTime(10*time.Millisecond), MaxIdleTime(5*time.Minute), Logger(testr.New(t)))

			for i := 0; i < tc.buildRequests; i++ {
				wp.requests.Enqueue(&PodRequest{result: make(chan PodRequestResult, 1)})
			}

			if err := wp.reconcileWorkers(context.Background()); err != nil {
				t.Error(err)
			}

			actual := <-updateCh
			if tc.expectedReplicas != actual {
				t.Errorf("got %d, want %d", actual, tc.expectedReplicas)
			}
		})
	}
}

func TestPoolClose(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	t.Run("Started", func(t *testing.T) {
		wp := NewPool(fakeClient, testConfig, SyncWaitTime(10*time.Millisecond), MaxIdleTime(5*time.Minute), Logger(testr.New(t)))
		if err := wp.Start(context.Background()); err != nil {
			t.Error(err)
		}
		wp.Close()
		<-wp.done
		if err := context.Cause(wp.stopCtx); err != errPoolClosed {
			t.Error(err)
		}
	})

	t.Run("ctx cancelled", func(t *testing.T) {
		wp := NewPool(fakeClient, testConfig, SyncWaitTime(10*time.Millisecond), MaxIdleTime(5*time.Minute), Logger(testr.New(t)))
		defer wp.Close()

		ctx, cancel := context.WithCancelCause(context.Background())
		if err := wp.Start(ctx); err != nil {
			t.Error(err)
		}
		myErr := errors.New("myErr")
		// Start should exit and done should be closed
		cancel(myErr)
		<-wp.done
		if err := context.Cause(wp.stopCtx); err != nil {
			t.Error(err)
		}
	})

	t.Run("Never started", func(t *testing.T) {
		wp := NewPool(fakeClient, testConfig, SyncWaitTime(10*time.Millisecond), MaxIdleTime(5*time.Minute), Logger(testr.New(t)))
		closedCh := make(chan struct{})
		go func() {
			wp.Close()
			close(closedCh)
		}()
		select {
		case <-closedCh:
			t.Error("unexpected")
		default:
		}
		wp.Start(context.Background())
		<-closedCh
	})

	t.Run("Close twice", func(t *testing.T) {
		wp := NewPool(fakeClient, testConfig, SyncWaitTime(10*time.Millisecond), MaxIdleTime(5*time.Minute), Logger(testr.New(t)))
		if err := wp.Start(context.Background()); err != nil {
			t.Error(err)
		}
		wp.Close()
		wp.Close()
	})
}

func validSts() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
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
}

func validEndpointSlice(pods ...runtime.Object) *discoveryv1.EndpointSlice {
	endpoints := make([]discoveryv1.Endpoint, 0, len(pods))
	for i := range pods {
		pod := pods[i].(*corev1.Pod)
		ready := pointer.Bool(false)
		if pod.Status.Phase == corev1.PodRunning {
			if idx := slices.IndexFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool { return c.Type == corev1.PodReady }); idx != -1 {
				ready = pointer.Bool(pod.Status.Conditions[idx].Status == corev1.ConditionTrue)
			}
			endpoints = append(endpoints, discoveryv1.Endpoint{
				Conditions: discoveryv1.EndpointConditions{
					Ready:   ready,
					Serving: ready,
				},
				Hostname: pointer.String(pod.Name),
				TargetRef: &corev1.ObjectReference{
					Name:      pod.Name,
					Namespace: namespace,
				},
			})
		}
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "buildkit-bgk87",
			Namespace: namespace,
		},
		Endpoints: endpoints,
		Ports: []discoveryv1.EndpointPort{
			{
				Name: pointer.String("daemon"),
				Port: pointer.Int32(1234),
			},
		},
	}
}

func validPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "buildkit-0",
			Namespace:         namespace,
			Labels:            testLabels,
			CreationTimestamp: metav1.Now(),
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
}

func leasedPod() *corev1.Pod {
	leased := validPod()
	leased.ObjectMeta.Annotations = map[string]string{
		leasedAtAnnotation:  time.Now().Format(time.RFC3339),
		leasedByAnnotation:  owner,
		managerIDAnnotation: string(newUUID()),
	}

	return leased
}

func unmanagedPod() *corev1.Pod {
	unmanaged := leasedPod()
	unmanaged.ObjectMeta.Annotations[managerIDAnnotation] = string(uuid.NewUUID())

	return unmanaged
}

func pendingPod() *corev1.Pod {
	pending := validPod()
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.PodScheduled,
			Status: corev1.ConditionFalse,
		},
		{
			Type:   corev1.PodInitialized,
			Status: corev1.ConditionFalse,
		},
		{
			Type:   corev1.ContainersReady,
			Status: corev1.ConditionFalse,
		},
		{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		},
	}

	return pending
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

// NOTE: this set of assertions is fine, but it's not great. we need a better way of asserting the patching.
//	ideally, we would make assertions against the API object after the event but client-go doesn't support SSA right
//	now, which means we have to override the "patch" action with a reactor.

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
