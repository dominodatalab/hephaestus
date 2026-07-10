package phase

import (
	"github.com/dominodatalab/controller-util/core"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

type PhasedObject interface {
	client.Object
	core.ConditionObject

	GetPhase() hephv1.Phase
	SetPhase(p hephv1.Phase)
}

type TransitionConditions struct {
	Initialize func() (string, string)
	Running    func() (string, string)
	Success    func() (string, string)
}

type TransitionHelper struct {
	Client         client.Client
	ConditionMeta  TransitionConditions
	ReadyCondition string
}

func (h *TransitionHelper) SetInitializing(ctx *core.Context, obj PhasedObject) {
	obj.SetPhase(hephv1.PhaseInitializing)

	reason, message := h.ConditionMeta.Initialize()
	ctx.Conditions.SetUnknown(h.ReadyCondition, reason, message)

	_ = h.updateStatus(ctx, obj)
}

// SetSucceeded transitions the object to the Succeeded phase and persists it, returning
// the status-write error (nil on success). Callers should treat a non-nil return as a
// non-durable terminal transition — the phase was not recorded and the reconcile should be
// retried before acting on the transition (e.g. emitting a terminal-outcome metric).
func (h *TransitionHelper) SetSucceeded(ctx *core.Context, obj PhasedObject) error {
	obj.SetPhase(hephv1.PhaseSucceeded)

	reason, message := h.ConditionMeta.Success()
	ctx.Conditions.SetTrue(h.ReadyCondition, reason, message)

	return h.updateStatus(ctx, obj)
}

func (h *TransitionHelper) SetRunning(ctx *core.Context, obj PhasedObject) {
	obj.SetPhase(hephv1.PhaseRunning)

	reason, message := h.ConditionMeta.Success()
	ctx.Conditions.SetUnknown(h.ReadyCondition, reason, message)

	_ = h.updateStatus(ctx, obj)
}

// SetFailed transitions the object to the Failed phase and persists it. It returns the
// status-write error (nil on success), NOT the passed-in build error: a non-nil return
// means the terminal transition was not durably recorded and the reconcile should be
// retried. err is used only to populate the failure condition message.
func (h *TransitionHelper) SetFailed(ctx *core.Context, obj PhasedObject, err error) error {
	obj.SetPhase(hephv1.PhaseFailed)
	ctx.Conditions.SetFalse(h.ReadyCondition, "ExecutionError", err.Error())

	return h.updateStatus(ctx, obj)
}

func (h *TransitionHelper) updateStatus(ctx *core.Context, obj PhasedObject) error {
	ctx.Log.Info("Transitioning status", "phase", obj.GetPhase())

	if err := ctx.Client.Status().Update(ctx, obj); err != nil {
		ctx.Log.Error(err, "Failed to update status, emitting event")
		ctx.Recorder.Eventf(
			obj,
			corev1.EventTypeWarning,
			"StatusUpdate",
			"Failed to update phase %s: %v", obj.GetPhase(), err,
		)

		return err
	}

	return nil
}
