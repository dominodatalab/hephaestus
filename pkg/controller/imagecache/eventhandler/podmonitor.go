package eventhandler

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
)

// NOTE: we can either use this object or a func defined on the cache warmer object

type PodMonitor struct {
	handler.Funcs

	Log        logr.Logger
	Client     client.Client
	Config     config.Buildkit
	TimeWindow time.Duration
}

func (p *PodMonitor) Create(e event.CreateEvent, q workqueue.RateLimitingInterface) {
	if len(p.Config.PodLabels) > len(e.Object.GetLabels()) {
		return
	}
	for k, v := range p.Config.PodLabels {
		if ov, found := e.Object.GetLabels()[k]; !found || ov != v {
			return
		}
	}

	// NOTE: work through the permutations
	ageLimit := time.Now().Add(-p.TimeWindow)
	if e.Object.GetCreationTimestamp().Time.Before(ageLimit) {
		return
	}

	cacheList := &hephv1.ImageCacheList{}
	err := p.Client.List(context.Background(), cacheList)
	if err != nil {
		p.Log.Error(err, "cannot list image cache objects")
	}

	for _, ic := range cacheList.Items {
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      ic.Name,
			Namespace: ic.Namespace,
		}})
	}
}
