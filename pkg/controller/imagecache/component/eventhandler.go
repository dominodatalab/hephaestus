package component

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
)

type PodMonitorEventHandler struct {
	log        logr.Logger
	client     client.Client
	config     config.Buildkit
	timeWindow time.Duration
}

func (p *PodMonitorEventHandler) Create(e event.CreateEvent, q workqueue.RateLimitingInterface) {
	if len(p.config.Labels) > len(e.Object.GetLabels()) {
		return
	}
	for k, v := range p.config.Labels {
		if ov, found := e.Object.GetLabels()[k]; !found || ov != v {
			return
		}
	}

	// NOTE: work through the permutations
	ageLimit := time.Now().Add(-p.timeWindow)
	if e.Object.GetCreationTimestamp().Time.Before(ageLimit) {
		return
	}

	cacheList := &hephv1.ImageCacheList{}
	err := p.client.List(context.Background(), cacheList)
	if err != nil {
		p.log.Error(err, "cannot list image cache objects")
	}

	for _, ic := range cacheList.Items {
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      ic.Name,
			Namespace: ic.Namespace,
		}})
	}
}

func (p *PodMonitorEventHandler) Update(e event.UpdateEvent, q workqueue.RateLimitingInterface) {}

func (p *PodMonitorEventHandler) Delete(e event.DeleteEvent, q workqueue.RateLimitingInterface) {}

func (p *PodMonitorEventHandler) Generic(e event.GenericEvent, q workqueue.RateLimitingInterface) {}
