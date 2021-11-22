package component

import (
	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"
)

type PodMonitorComponent struct{}

func PodMonitor() *PodMonitorComponent {
	return &PodMonitorComponent{}
}

func (c *PodMonitorComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	// TODO: this component will watch buildkit pods and trigger another cache
	//  operation whenever new pods come up to ensure the cache is consistent
	//  across the entire cluster.

	return ctrl.Result{}, nil
}
