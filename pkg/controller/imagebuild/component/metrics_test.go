package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/phase"
)

func TestRecordImageBuildPhase(t *testing.T) {
	imageBuildPhaseTotal.Reset()
	t.Cleanup(imageBuildPhaseTotal.Reset)

	// Two failures of the same reason, one of another, and one success.
	recordImageBuildPhase(hephv1.PhaseFailed, "CredentialsValidateError")
	recordImageBuildPhase(hephv1.PhaseFailed, "CredentialsValidateError")
	recordImageBuildPhase(hephv1.PhaseFailed, "ImageBuildError")
	recordImageBuildPhase(hephv1.PhaseSucceeded, "")

	expected := `
# HELP hephaestus_imagebuild_phase_total Count of ImageBuild terminal phase transitions, labeled by phase and failure reason.
# TYPE hephaestus_imagebuild_phase_total counter
hephaestus_imagebuild_phase_total{failure_reason="",phase="Succeeded"} 1
hephaestus_imagebuild_phase_total{failure_reason="CredentialsValidateError",phase="Failed"} 2
hephaestus_imagebuild_phase_total{failure_reason="ImageBuildError",phase="Failed"} 1
`

	if err := testutil.CollectAndCompare(imageBuildPhaseTotal, strings.NewReader(expected)); err != nil {
		t.Errorf("unexpected metric state:\n%v", err)
	}
}

// newPhaseTestCtx builds the minimal core.Context the phase helper needs (client,
// condition helper, recorder, logger) around a fake client.
func newPhaseTestCtx(cl client.Client, obj *hephv1.ImageBuild) *core.Context {
	return &core.Context{
		Context:    context.Background(),
		Log:        logr.Discard(),
		Object:     obj,
		Client:     cl,
		Scheme:     scheme(),
		Recorder:   record.NewFakeRecorder(16),
		Conditions: core.NewConditionHelper(obj),
	}
}

func newPhaseTestDispatcher(cl client.Client) *BuildDispatcherComponent {
	return &BuildDispatcherComponent{
		phase: &phase.TransitionHelper{
			Client: cl,
			ConditionMeta: phase.TransitionConditions{
				Initialize: func() (string, string) { return "Setup", "Processing build parameters" },
				Running:    func() (string, string) { return "BuildingImage", "Running image build in buildkit" },
				Success:    func() (string, string) { return "BuildComplete", "Image built and pushed" },
			},
			ReadyCondition: "ImageReady",
		},
	}
}

// flakyStatusClient wraps base so the first failUpdates status-write attempts fail
// (mimicking API-server conflicts during an outage) and subsequent attempts succeed.
func flakyStatusClient(t *testing.T, base client.WithWatch, failUpdates int) client.Client {
	t.Helper()
	attempts := 0
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, subResourceName string,
			obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			attempts++
			if attempts <= failUpdates {
				return errors.New("status update failed: simulated conflict")
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	})
}

// TestFailBuildCountsOnlyOnDurableTransition is the regression test for the ticket's
// "no double-counting under reconcile requeue" acceptance criterion. When the status
// write fails, the terminal transition is not durable and the reconcile requeues; the
// metric must NOT increment until the write finally lands, and then exactly once.
func TestFailBuildCountsOnlyOnDurableTransition(t *testing.T) {
	imageBuildPhaseTotal.Reset()
	t.Cleanup(imageBuildPhaseTotal.Reset)

	obj := &hephv1.ImageBuild{
		TypeMeta:   metav1.TypeMeta{Kind: ibGVK.Kind, APIVersion: hephv1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "aloha"},
		Status:     hephv1.ImageBuildStatus{Phase: hephv1.PhaseRunning},
	}
	base := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
	cl := flakyStatusClient(t, base, 2) // first two requeues fail to persist, third succeeds

	c := newPhaseTestDispatcher(cl)
	buildErr := errors.New("buildkit service lookup failed")

	labels := imageBuildPhaseTotal.WithLabelValues(string(hephv1.PhaseFailed), "WorkerLeaseError")

	// Two reconcile passes whose status write fails: no increment, error surfaced for requeue.
	for pass := 1; pass <= 2; pass++ {
		ctx := newPhaseTestCtx(cl, obj)
		if err := c.failBuild(ctx, obj, buildErr, "WorkerLeaseError"); err == nil {
			t.Fatalf("pass %d: expected non-nil error when status write fails", pass)
		}
		if got := testutil.ToFloat64(labels); got != 0 {
			t.Fatalf("pass %d: counter incremented on non-durable transition: got %v, want 0", pass, got)
		}
	}

	// Third pass persists: increment exactly once, original build error returned.
	ctx := newPhaseTestCtx(cl, obj)
	if err := c.failBuild(ctx, obj, buildErr, "WorkerLeaseError"); !errors.Is(err, buildErr) {
		t.Fatalf("expected original build error on durable transition, got: %v", err)
	}
	if got := testutil.ToFloat64(labels); got != 1 {
		t.Fatalf("counter after durable transition: got %v, want 1", got)
	}
}

// TestSucceedBuildCountsOnlyOnDurableTransition mirrors the failure-path regression test
// for the success terminal transition.
func TestSucceedBuildCountsOnlyOnDurableTransition(t *testing.T) {
	imageBuildPhaseTotal.Reset()
	t.Cleanup(imageBuildPhaseTotal.Reset)

	obj := &hephv1.ImageBuild{
		TypeMeta:   metav1.TypeMeta{Kind: ibGVK.Kind, APIVersion: hephv1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "b2", Namespace: "aloha"},
		Status:     hephv1.ImageBuildStatus{Phase: hephv1.PhaseRunning},
	}
	base := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
	cl := flakyStatusClient(t, base, 1) // first requeue fails to persist, second succeeds

	c := newPhaseTestDispatcher(cl)
	labels := imageBuildPhaseTotal.WithLabelValues(string(hephv1.PhaseSucceeded), "")

	ctx := newPhaseTestCtx(cl, obj)
	if err := c.succeedBuild(ctx, obj); err == nil {
		t.Fatal("expected non-nil error when status write fails")
	}
	if got := testutil.ToFloat64(labels); got != 0 {
		t.Fatalf("counter incremented on non-durable transition: got %v, want 0", got)
	}

	ctx = newPhaseTestCtx(cl, obj)
	if err := c.succeedBuild(ctx, obj); err != nil {
		t.Fatalf("expected nil error on durable transition, got: %v", err)
	}
	if got := testutil.ToFloat64(labels); got != 1 {
		t.Fatalf("counter after durable transition: got %v, want 1", got)
	}
}

// TestStaleCacheDoesNotDoubleCount covers the case where, after a build's terminal Failed
// phase is durably persisted with its real reason, a subsequent reconcile reads a STALE
// informer cache that still shows Running/Initializing and therefore re-enters the
// NotRunning branch. The stale object carries an out-of-date resourceVersion, so its
// status write is rejected with a 409 Conflict; because the metric increments only after a
// successful write, the NotRunning increment never happens and the reason is not
// double-counted. This is protection via optimistic concurrency — it does not depend on
// the cache being up to date.
func TestStaleCacheDoesNotDoubleCount(t *testing.T) {
	imageBuildPhaseTotal.Reset()
	t.Cleanup(imageBuildPhaseTotal.Reset)

	key := types.NamespacedName{Namespace: "aloha", Name: "b3"}
	obj := &hephv1.ImageBuild{
		TypeMeta:   metav1.TypeMeta{Kind: ibGVK.Kind, APIVersion: hephv1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Status:     hephv1.ImageBuildStatus{Phase: hephv1.PhaseRunning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(obj).WithStatusSubresource(obj).Build()
	c := newPhaseTestDispatcher(cl)
	ctx := context.Background()

	// A stale cache snapshot at the pre-terminal (Running) resourceVersion.
	stale := &hephv1.ImageBuild{}
	if err := cl.Get(ctx, key, stale); err != nil {
		t.Fatal(err)
	}

	// The real reconcile object (fresh) fails with the real reason and durably persists Failed.
	fresh := &hephv1.ImageBuild{}
	if err := cl.Get(ctx, key, fresh); err != nil {
		t.Fatal(err)
	}
	err := c.failBuild(newPhaseTestCtx(cl, fresh), fresh, errNotRunning, "CredentialsValidateError")
	if !errors.Is(err, errNotRunning) {
		t.Fatalf("expected build error on durable transition, got: %v", err)
	}

	realReason := imageBuildPhaseTotal.WithLabelValues(string(hephv1.PhaseFailed), "CredentialsValidateError")
	notRunning := imageBuildPhaseTotal.WithLabelValues(string(hephv1.PhaseFailed), "NotRunning")
	if got := testutil.ToFloat64(realReason); got != 1 {
		t.Fatalf("real-reason counter after durable transition: got %v, want 1", got)
	}

	// The stale-cache reconcile re-enters the NotRunning branch; its write must 409, so no increment.
	persistErr := c.failBuild(newPhaseTestCtx(cl, stale), stale, errNotRunning, "NotRunning")
	if !apierrors.IsConflict(persistErr) {
		t.Fatalf("expected 409 Conflict from stale-resourceVersion write, got: %v", persistErr)
	}
	if got := testutil.ToFloat64(notRunning); got != 0 {
		t.Fatalf("NotRunning double-counted from stale cache: got %v, want 0", got)
	}
	if got := testutil.ToFloat64(realReason); got != 1 {
		t.Fatalf("real-reason counter changed unexpectedly: got %v, want 1", got)
	}
}
