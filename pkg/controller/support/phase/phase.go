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

	GetPatch() client.Patch
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

	h.patchStatus(ctx, obj)
}

func (h *TransitionHelper) SetSucceeded(ctx *core.Context, obj PhasedObject) {
	obj.SetPhase(hephv1.PhaseSucceeded)

	reason, message := h.ConditionMeta.Success()
	ctx.Conditions.SetTrue(h.ReadyCondition, reason, message)

	h.patchStatus(ctx, obj)
}

func (h *TransitionHelper) SetRunning(ctx *core.Context, obj PhasedObject) {
	obj.SetPhase(hephv1.PhaseRunning)

	reason, message := h.ConditionMeta.Success()
	ctx.Conditions.SetUnknown(h.ReadyCondition, reason, message)

	h.patchStatus(ctx, obj)
}

func (h *TransitionHelper) SetFailed(ctx *core.Context, obj PhasedObject, err error) error {
	obj.SetPhase(hephv1.PhaseFailed)
	ctx.Conditions.SetFalse(h.ReadyCondition, "ExecutionError", err.Error())

	h.patchStatus(ctx, obj)

	return err
}

func (h *TransitionHelper) patchStatus(ctx *core.Context, obj PhasedObject) {
	ctx.Log.Info("Transitioning status", "phase", obj.GetPhase())

	if err := ctx.Client.Status().Patch(ctx, obj, obj.GetPatch()); err != nil {
		ctx.Log.Error(err, "Failed to update status, emitting event")
		ctx.Recorder.Eventf(
			obj,
			corev1.EventTypeWarning,
			"StatusUpdate",
			"Failed to update phase %s: %w", obj.GetPhase(), err,
		)
	}
}
