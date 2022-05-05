package workerpool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

var ErrNoLeases = errors.New("idle lease not found")

type AddressFuture func() (endpoint string, err error)

type Pool interface {
	Get(ctx context.Context) (AddressFuture, error)
	Release(endpoint string) error
}

type workerLease struct {
	Addr string    `json:"addr"`
	Idle bool      `json:"idle"`
	Age  time.Time `json:"age"`
}

type workerPool struct {
	lmu    sync.Mutex
	smu    sync.Mutex
	leases []*workerLease

	log                logr.Logger
	conf               config.Buildkit
	clientset          kubernetes.Interface
	maxIdleTime        time.Duration
	syncWaitTime       time.Duration
	podListOptions     metav1.ListOptions
	watchTimoutSeconds *int64
}

func New(ctx context.Context, restCfg *rest.Config, cfg config.Buildkit, opts ...PoolOption) (*workerPool, error) {
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("cannot create kubernetes clientset: %w", err)
	}

	sts, err := clientset.AppsV1().StatefulSets(cfg.Namespace).Get(ctx, cfg.StatefulSetName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return nil, err
	}

	o := defaultOpts
	for _, fn := range opts {
		o = fn(o)
	}

	pool := &workerPool{
		conf:         cfg,
		clientset:    clientset,
		log:          o.log.WithName("worker-pool"),
		maxIdleTime:  o.maxIdleTime,
		syncWaitTime: o.syncWaitTime,
		podListOptions: metav1.ListOptions{
			LabelSelector: selector.String(),
			FieldSelector: "status.phase=Running",
		},
	}
	go pool.monitorWorkers(ctx)

	return pool, nil
}

func (p *workerPool) Get(ctx context.Context) (AddressFuture, error) {
	addr, err := p.leaseAddr(ctx)

	var fn AddressFuture
	switch {
	case err == nil:
		fn = func() (string, error) {
			return addr, nil
		}
	case errors.Is(err, ErrNoLeases):
		fn = func() (string, error) {
			if err = p.scaleStatefulSet(ctx, 1); err != nil {
				return "", err
			}

			if addr, err = p.leaseAddr(ctx); err != nil {
				return "", fmt.Errorf("unable to acquire lease after scaling: %w", err)
			}

			return addr, nil
		}
	default:
		return nil, err
	}

	return fn, nil
}

func (p *workerPool) Release(addr string) error {
	p.lmu.Lock()
	defer p.lmu.Unlock()

	for _, lease := range p.leases {
		if lease.Addr == addr {
			lease.Idle = true
			return nil
		}
	}

	return fmt.Errorf("addr %q is not allocated", addr)
}

func (p *workerPool) leaseAddr(ctx context.Context) (string, error) {
	p.lmu.Lock()
	defer p.lmu.Unlock()

	if err := p.syncLeases(ctx); err != nil {
		return "", err
	}

	for _, lease := range p.leases {
		if lease.Idle {
			lease.Idle = false
			return lease.Addr, nil
		}
	}

	return "", ErrNoLeases
}

func (p *workerPool) syncLeases(ctx context.Context) error {
	leases, err := p.loadEndpointsIntoLeases(ctx)
	if err != nil {
		return err
	}

	currentLeaseMap := make(map[string]workerLease)
	for _, lease := range p.leases {
		currentLeaseMap[lease.Addr] = *lease
	}

	for _, lease := range leases {
		if existing, ok := currentLeaseMap[lease.Addr]; ok {
			lease.Idle = existing.Idle
		} else {
			lease.Idle = true
		}
	}
	p.leases = leases

	return nil
}

func (p *workerPool) loadEndpointsIntoLeases(ctx context.Context) ([]*workerLease, error) {
	p.log.V(1).Info("Querying for endpoints", "name", p.conf.ServiceName, "namespace", p.conf.Namespace)
	ep, err := p.clientset.CoreV1().Endpoints(p.conf.Namespace).Get(ctx, p.conf.ServiceName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	podAgeMap, err := p.getPodCreationTimestamps(ctx)
	if err != nil {
		return nil, err
	}

	var leases []*workerLease

	for _, subset := range ep.Subsets {
		var targetPortPresent bool

		for _, port := range subset.Ports {
			if port.Port == p.conf.DaemonPort {
				targetPortPresent = true
				break
			}
		}

		if !targetPortPresent {
			return nil, fmt.Errorf("endpoint %q does not expose daemon port %d", ep.Name, p.conf.DaemonPort)
		}

		for _, addr := range subset.Addresses {
			dns := strings.Join([]string{addr.Hostname, ep.Name, ep.Namespace}, ".")
			lease := &workerLease{
				Addr: fmt.Sprintf("tcp://%s:%d", dns, p.conf.DaemonPort),
				Age:  podAgeMap[addr.TargetRef.Name],
			}

			leases = append(leases, lease)
		}
	}

	p.log.V(1).Info("Generated raw lease data", "leases", leases)
	return leases, nil
}

func (p *workerPool) getPodCreationTimestamps(ctx context.Context) (map[string]time.Time, error) {
	p.log.V(1).Info("Querying for pods", "namespace", p.conf.Namespace, "opts", p.podListOptions)
	podList, err := p.clientset.CoreV1().Pods(p.conf.Namespace).List(ctx, p.podListOptions)
	if err != nil {
		return nil, err
	}

	podMap := make(map[string]time.Time)
	for _, pod := range podList.Items {
		podMap[pod.Name] = pod.CreationTimestamp.Time
	}

	p.log.V(1).Info("Created pod name->age mapping", "map", podMap)
	return podMap, nil
}

// scaleStatefulSet will scale up/down the buildkit cluster using the provided count.
//
// Concurrent lease requests that require a scaling action to provide unleased
// addresses may cause the cluster to scale up by more than 1 worker. A
// dedicated mutex ensures that watch conditions are accurate when multiple
// AddressFuture functions are invoked simultaneously.
func (p *workerPool) scaleStatefulSet(ctx context.Context, count int32) error {
	stsAPI := p.clientset.AppsV1().StatefulSets(p.conf.Namespace)

	p.smu.Lock()
	locked := true

	defer func() {
		if locked {
			p.smu.Unlock()
		}
	}()

	p.log.V(1).Info("Querying for statefulset", "name", p.conf.StatefulSetName)
	sts, err := stsAPI.Get(ctx, p.conf.StatefulSetName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot get statefulset: %w", err)
	}

	replicas := pointer.Int32Deref(sts.Spec.Replicas, 0) + count
	jsonPatch := fmt.Sprintf(`{"spec": {"replicas": %d}}`, replicas)

	p.log.V(1).Info("Patching statefulset", "name", p.conf.StatefulSetName, "patch", jsonPatch)
	_, err = stsAPI.Patch(ctx, p.conf.StatefulSetName, types.StrategicMergePatchType, []byte(jsonPatch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("cannot patch replicas: %w", err)
	}

	p.smu.Unlock()
	locked = false

	listOpts := metav1.ListOptions{
		LabelSelector:  labels.FormatLabels(sts.Labels),
		TimeoutSeconds: p.watchTimoutSeconds,
	}

	p.log.V(1).Info("Starting statefulset watch", "opts", listOpts)
	watcher, err := stsAPI.Watch(ctx, listOpts)
	if err != nil {
		return fmt.Errorf("cannot watch statefulset: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		target := event.Object.(*appsv1.StatefulSet)
		if target.Status.ReadyReplicas >= replicas {
			p.log.V(1).Info("Stateful replicas ready, stopping watch")
			break
		} else {
			p.log.V(1).Info("Stateful replicas not ready, still watching", "expected", replicas, "actual", target.Status.ReadyReplicas)
		}
	}

	return nil
}

func (p *workerPool) monitorWorkers(ctx context.Context) {
	ticker := time.NewTicker(p.syncWaitTime)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.log.V(1).Info("Reaping worker pool")

			if err := p.reapWorkerPool(ctx); err != nil {
				p.log.Error(err, "Failed to reap worker pool")
			}
		case <-ctx.Done():
			p.log.V(1).Info("Shutting down worker pool monitor")
			return
		}
	}
}

func (p *workerPool) reapWorkerPool(ctx context.Context) error {
	p.lmu.Lock()
	defer p.lmu.Unlock()

	if err := p.syncLeases(ctx); err != nil {
		return err
	}

	var count int32
	for i := len(p.leases) - 1; i >= 0; i-- {
		lease := p.leases[i]

		if lease.Idle && time.Since(lease.Age) > p.maxIdleTime {
			p.log.V(1).Info("Scheduling builder for removal", "addr", lease.Addr, "age", lease.Age)
			count--
		} else {
			break
		}
	}

	if count < 0 {
		p.log.V(1).Info("Scaling down cluster")
		if err := p.scaleStatefulSet(ctx, count); err != nil {
			return err
		}
	}

	return nil
}
