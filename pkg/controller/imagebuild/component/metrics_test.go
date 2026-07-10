package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
