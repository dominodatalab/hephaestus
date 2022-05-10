package component

import (
	"github.com/dominodatalab/controller-util/core"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

type DeleteBroadcastComponent struct {
	delete chan<- client.ObjectKey
}

func DeleteBroadcaster(ch chan<- client.ObjectKey) *DeleteBroadcastComponent {
	return &DeleteBroadcastComponent{
		delete: ch,
	}
}

func (c *DeleteBroadcastComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageBuild)
	err := ctx.Client.Get(ctx, obj.ObjectKey(), &hephv1.ImageBuild{})

	if err == nil {
		return ctrl.Result{}, nil
	}

	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	log.Info("Broadcasting delete message")
	c.delete <- obj.ObjectKey()

	return ctrl.Result{}, nil
}

// ctx, cancel := context.WithCancel(coreCtx)
// c.cancels.Store(obj.ObjectKey(), cancel)
// defer func() {
// 	cancel()
// 	c.cancels.Delete(obj.ObjectKey())
// }()
