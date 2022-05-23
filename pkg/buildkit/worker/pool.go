package worker

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

type Pool interface {
	Get(ctx context.Context) (workerAddr string, err error)
	Release(ctx context.Context, workerAddr string) error
	Close()
}

type workerPool struct {
	ctx    context.Context
	cancel context.CancelFunc

	log      logr.Logger
	plm      LeaseManager
	outgoing RequestQueue
	incoming chan LeaseRequest
}

func NewPool(ctx context.Context, clientset kubernetes.Interface, conf config.Buildkit, opts ...PoolOption) Pool {
	o := defaultOpts
	for _, fn := range opts {
		o = fn(o)
	}
	log := o.log.WithName("worker-pool")

	scaler := NewStatefulSetScaler(ScalerOpt{
		Log:                 log.WithName("statefulset-scaler"),
		Name:                conf.StatefulSetName,
		Namespace:           conf.Namespace,
		Clientset:           clientset,
		WatchTimeoutSeconds: o.watchTimeoutSeconds,
	})

	leaseManager := NewPodLeaseManager(ctx, LeaseManagerOpt{
		Log:            log.WithName("lease-manager"),
		Clienset:       clientset,
		Namespace:      conf.Namespace,
		PodLabels:      conf.PodLabels,
		ServiceName:    conf.ServiceName,
		ServicePort:    conf.DaemonPort,
		WorkloadScaler: scaler,
	})

	ctx, cancel := context.WithCancel(ctx)
	wp := &workerPool{
		ctx:      ctx,
		cancel:   cancel,
		log:      log,
		plm:      leaseManager,
		incoming: make(chan LeaseRequest),
		outgoing: NewRequestQueue(),
	}
	go wp.processIncoming()

	return wp
}

func (p *workerPool) Get(ctx context.Context) (string, error) {
	ch := make(chan LeaseRequest, 1)
	p.outgoing.Enqueue(ch)

	go p.dispatchRequest(ctx)

	result := <-ch
	return result.addr, result.err
}

func (p *workerPool) Release(ctx context.Context, addr string) error {
	return p.plm.Release(ctx, addr)
}

func (p *workerPool) Close() {
	p.cancel()
}

func (p *workerPool) dispatchRequest(ctx context.Context) {
	addr, err := p.plm.Lease(ctx)
	p.incoming <- LeaseRequest{addr: addr, err: err}
}

func (p *workerPool) processIncoming() {
	for {
		select {
		case <-p.ctx.Done():
			close(p.incoming)
			for p.outgoing.Len() > 1 {
				close(p.outgoing.Dequeue())
			}

			return
		case result := <-p.incoming:
			out := p.outgoing.Dequeue()
			out <- result
			close(out)
		}
	}
}
