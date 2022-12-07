package component

import (
	"context"
	"testing"
	"time"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBuildDispatcherComponentIncomingCancellations(t *testing.T) {
	targetCtx, targetCancel := context.WithCancel(context.Background())
	bystanderCtx, bystanderCancel := context.WithCancel(context.Background())
	deleteCh := make(chan client.ObjectKey)

	comp := &BuildDispatcherComponent{
		delete: deleteCh,
	}

	targetObjKey := client.ObjectKey{
		Namespace: "test-ns",
		Name:      "target",
	}
	comp.cancels.Store(targetObjKey, targetCancel)
	comp.cancels.Store(client.ObjectKey{
		Namespace: "test-ns",
		Name:      "bystander",
	}, bystanderCancel)

	require.NoError(t, comp.Initialize(&core.Context{Log: logr.Discard()}, nil))

	deleteCh <- targetObjKey

	select {
	case <-targetCtx.Done():
		assert.ErrorIs(t, targetCtx.Err(), context.Canceled)
	case <-time.After(5 * time.Second):
		assert.Fail(t, "target context was not canceled before timeout")
	}
	assert.NoError(t, bystanderCtx.Err(), "bystander context was erroneously canceled")
}

func TestBuildDispatcherComponentReconciliation(t *testing.T) {
	t.Skip("todo")
	// ctx := &core.Context{
	// 	Context:    nil,
	// 	Log:        logr.Discard(),
	// 	Data:       nil,
	// 	Patch:      nil,
	// 	Object:     nil,
	// 	Config:     nil,
	// 	Client:     nil,
	// 	Scheme:     nil,
	// 	Recorder:   nil,
	// 	Conditions: nil,
	// }
	// comp := BuildDispatcher(config.Buildkit{}, nil, nil, nil)
	//
	// comp.Reconcile(ctx)
}
