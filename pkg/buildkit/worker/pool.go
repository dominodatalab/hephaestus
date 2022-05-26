package worker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

type Pool interface {
	Get(ctx context.Context) (workerAddr string, err error)
	Release(ctx context.Context, workerAddr string) error
	Close()
}

var (
	errNoUnleasedPods = errors.New("no unleased pods found")
	statefulPodRegex  = regexp.MustCompile(`^.*-(\d+)$`)
)

const (
	fieldManagerName     = "hephaestus-pod-lease-manager"
	leasedAnnotation     = "hephaestus.dominodatalab.com/leased"
	managerIDAnnotation  = "hephaestus.dominodatalab.com/manager-identity"
	expiryTimeAnnotation = "hephaestus.dominodatalab.com/expiry-time"
)

type workerPool struct {
	log logr.Logger

	// system shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// incoming/outgoing lease requests/results
	requests RequestQueue
	results  chan PodRequestResult

	// gc routine
	poolSyncTime   time.Duration
	podMaxIdleTime time.Duration

	// leasing
	mu              sync.Mutex
	uuid            string
	namespace       string
	podClient       corev1typed.PodInterface
	endpointsClient corev1typed.EndpointsInterface
	podListOptions  metav1.ListOptions
	scaler          Scaler

	// endpoints discovery
	serviceName string
	servicePort int32
}

func NewPool(ctx context.Context, clientset kubernetes.Interface, conf config.Buildkit, opts ...PoolOption) Pool {
	o := defaultOpts
	for _, fn := range opts {
		o = fn(o)
	}

	scaler := NewStatefulSetScaler(ScalerOpt{
		Name:                conf.StatefulSetName,
		Namespace:           conf.Namespace,
		Clientset:           clientset,
		WatchTimeoutSeconds: o.watchTimeoutSeconds,
		Log:                 o.log.WithName("statefulset-scaler"),
	})

	fs := fields.SelectorFromSet(map[string]string{"status.phase": "Running"})
	ls := labels.SelectorFromSet(conf.PodLabels)
	podListOptions := metav1.ListOptions{
		LabelSelector: ls.String(),
		FieldSelector: fs.String(),
	}

	ctx, cancel := context.WithCancel(ctx)
	wp := &workerPool{
		ctx:             ctx,
		cancel:          cancel,
		log:             o.log,
		poolSyncTime:    o.syncWaitTime,
		podMaxIdleTime:  o.maxIdleTime,
		uuid:            string(uuid.NewUUID()),
		requests:        NewRequestQueue(),
		results:         make(chan PodRequestResult),
		podClient:       clientset.CoreV1().Pods(conf.Namespace),
		endpointsClient: clientset.CoreV1().Endpoints(conf.Namespace),
		podListOptions:  podListOptions,
		serviceName:     conf.ServiceName,
		servicePort:     conf.DaemonPort,
		namespace:       conf.Namespace,
		scaler:          scaler,
	}

	go wp.processRequests()
	go wp.monitorPool()

	return wp
}

func (p *workerPool) Get(ctx context.Context) (string, error) {
	ch := make(chan PodRequestResult, 1)
	defer close(ch)

	p.requests.Enqueue(ch)

	oob, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		pod, err := p.get(oob)
		if oob.Err() != nil {
			p.log.V(1).Info("OOB lease get cancelled, exiting")
			return
		}

		p.results <- PodRequestResult{pod, err}
		p.log.V(1).Info("OOB lease get results sent")
	}()

	result := <-ch
	if result.err != nil {
		return "", result.err
	}

	return p.buildEndpointURL(ctx, result.pod.Name)
}

func (p *workerPool) Release(ctx context.Context, addr string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log.Info("Parsing lease addr", "addr", addr)
	u, err := url.ParseRequestURI(addr)
	if err != nil {
		return fmt.Errorf("failed to parse lease addr: %w", err)
	}

	podName := strings.Split(u.Host, ".")[0]

	p.log.Info("Querying for pod", "name", podName, "namespace", p.namespace)
	pod, err := p.podClient.Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			err = fmt.Errorf("addr %q is not allocated: %w", addr, err)
		}
		return err
	}

	pac, err := corev1ac.ExtractPod(pod, fieldManagerName)
	if err != nil {
		return fmt.Errorf("cannot extract pod config: %w", err)
	}

	pac.WithAnnotations(map[string]string{
		expiryTimeAnnotation: time.Now().Add(p.podMaxIdleTime).Format(time.RFC3339),
	})
	delete(pac.Annotations, leasedAnnotation)
	delete(pac.Annotations, managerIDAnnotation)

	p.log.Info("Applying pod metadata changes", "annotations", pac.Annotations)
	if pod, err = p.podClient.Apply(ctx, pac, metav1.ApplyOptions{FieldManager: fieldManagerName}); err != nil {
		return fmt.Errorf("cannot update pod metadata: %w", err)
	}

	if request := p.requests.Dequeue(); request != nil {
		request <- PodRequestResult{pod, nil}
	}

	return nil
}

// Close shuts down the pool by terminating all background routines used to manage requests and garbage collection.
func (p *workerPool) Close() {
	p.cancel()
}

// coordinates lease and scaling actions
func (p *workerPool) get(ctx context.Context) (*corev1.Pod, error) {
	pod, err := p.leasePod(ctx)
	if err == nil {
		return pod, nil
	}
	if !errors.Is(err, errNoUnleasedPods) {
		return nil, err
	}

	p.log.Info("Scaling cluster, no unleased pods found")
	if err = p.scaler.Scale(ctx, 1); err != nil {
		return nil, err
	}
	p.log.Info("Scale cluster complete")

	pod, err = p.leasePod(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to acquire lease after scaling: %w", err)
	}

	return pod, nil
}

// finds unleased pod and applies lease metadata
func (p *workerPool) leasePod(ctx context.Context) (*corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log.Info("Querying for pods", "namespace", p.namespace, "opts", p.podListOptions)
	podList, err := p.podClient.List(ctx, p.podListOptions)
	if err != nil {
		return nil, err
	}

	var pod *corev1.Pod
	for _, item := range podList.Items {
		if _, ok := item.Annotations[leasedAnnotation]; !ok {
			p.log.Info("Found unleased pod", "name", item.Name, "namespace", item.Namespace)

			pod = &item
			break
		}
	}

	if pod == nil {
		p.log.Info("No unleased pods found")
		return nil, errNoUnleasedPods
	}

	pac, err := corev1ac.ExtractPod(pod, fieldManagerName)
	if err != nil {
		return nil, fmt.Errorf("cannot extract pod config: %w", err)
	}

	pac.WithAnnotations(map[string]string{
		leasedAnnotation:    "true",
		managerIDAnnotation: p.uuid,
	})
	delete(pac.Annotations, expiryTimeAnnotation)

	p.log.Info("Applying pod metadata changes", "annotations", pac.Annotations)
	if pod, err = p.podClient.Apply(ctx, pac, metav1.ApplyOptions{FieldManager: fieldManagerName}); err != nil {
		return nil, fmt.Errorf("cannot update pod metadata: %w", err)
	}

	return pod, nil
}

// builds routable url for buildkit pod with protocol and port
func (p *workerPool) buildEndpointURL(ctx context.Context, podName string) (string, error) {
	p.log.Info("Querying for endpoints", "name", p.serviceName, "namespace", p.namespace)
	endpoints, err := p.endpointsClient.Get(ctx, p.serviceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	hostname, err := p.extractHostname(endpoints, podName)
	if err != nil {
		return "", err
	}

	u, err := url.ParseRequestURI(fmt.Sprintf("tcp://%s:%d", hostname, p.servicePort))
	if err != nil {
		return "", fmt.Errorf("failed to parse endpoint url: %w", err)
	}

	return u.String(), nil
}

// generates internal hostname for pod using endpoints
func (p *workerPool) extractHostname(endpoints *corev1.Endpoints, podName string) (string, error) {
	var hostname string

scan:
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.TargetRef.Name == podName {
				for _, port := range subset.Ports {
					if port.Port == p.servicePort {
						hostname = strings.Join([]string{address.Hostname, endpoints.Name, endpoints.Namespace}, ".")
						p.log.Info("Found eligible endpoint address", "hostname", hostname)

						break scan
					}
				}
			}
		}
	}

	if hostname == "" {
		return "", fmt.Errorf("endpoints %q does not expose pod %s on port %d", endpoints.Name, podName, p.servicePort)
	}

	return hostname, nil
}

// async unleased pod request routing
func (p *workerPool) processRequests() {
	for {
		select {
		case <-p.ctx.Done():
			close(p.results)
			for p.requests.Len() > 1 {
				close(p.requests.Dequeue())
			}

			return
		case result := <-p.results:
			request := p.requests.Dequeue()
			request <- result
		}
	}
}

// async pool reaping routine
func (p *workerPool) monitorPool() {
	p.log.Info("Starting worker pod monitor", "syncTime", p.poolSyncTime.String())

	ticker := time.NewTicker(p.poolSyncTime)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			p.log.Info("Shutting down worker pod monitor")
			return
		case <-ticker.C:
			p.log.Info("Checking worker pool for expired leases")
			if err := p.terminateUnleasedPods(p.ctx); err != nil {
				p.log.Error(err, "Failed to clean worker pool")
			}
			p.log.Info("Worker pool lease check complete")
		}
	}
}

// pool garbage collection logic
//
// leasing is performed using metadata annotations by manipulating the following values:
//
// annotation	 | add lease	| remove lease
// "leased"		 | ADD			| REMOVE
// "manager-id"	 | ADD			| REMOVE
// "expiry-time" | REMOVE		| ADD
//
// termination order:
//
// statefulset pods (pod-0, pod-1, ..., pod-N) must be scaled up/down in order. this means that we cannot scale down
// unleased pod-N until all higher ordinal pods (pod-M where M > N) are unleased. hence, we check pods in reverse
// ordinal order and stop the scale down whenever we encounter a pod that is NOT eligible for termination.
//
// termination rules:
//
// 1. if a pod is unleased, has no expiry time, and is older than the max idle time, then it will be terminated.
// 2. if a pod is leased and its expiry time has passed, then it will be terminated.
// 3. if the controller is restarted while pods are leased, then they will have "manager-id" that matches the uuid of
//	  the previous instance and these leased pods will be terminated.
func (p *workerPool) terminateUnleasedPods(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log.Info("Querying for pods", "namespace", p.namespace, "opts", p.podListOptions)
	podList, err := p.podClient.List(ctx, p.podListOptions)
	if err != nil {
		return err
	}

	sort.Slice(podList.Items, func(i, j int) bool {
		return getOrdinal(podList.Items[i].Name) > getOrdinal(podList.Items[j].Name)
	})

	var removals []string
	for _, pod := range podList.Items {
		log := p.log.WithValues("podName", pod.Name)

		if id, ok := pod.Annotations[managerIDAnnotation]; ok && id != p.uuid {
			log.Info("Eligible for termination, manager id mismatch", "expected", p.uuid, "actual", id)
			removals = append(removals, pod.Name)

			continue
		}

		if _, leased := pod.Annotations[leasedAnnotation]; leased {
			break
		}

		ts, ok := pod.Annotations[expiryTimeAnnotation]
		if !ok && time.Since(pod.CreationTimestamp.Time) > p.podMaxIdleTime {
			log.Info("Eligible for termination, missing expiry time")
			removals = append(removals, pod.Name)

			continue
		}

		expiry, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return fmt.Errorf("failed to parse pod expiry time: %w", err)
		}

		if time.Now().After(expiry) {
			log.Info("Eligible for termination, ttl has expired", "expiry", expiry)
			removals = append(removals, pod.Name)
		}
	}

	count := -int32(len(removals))
	if count == 0 {
		p.log.Info("No pods eligible for termination")
		return nil
	}

	p.log.Info("Attempting to terminate pods via scale", "names", removals)
	return p.scaler.Scale(ctx, count)
}

// plucks the ordinal suffix off of a statefulset pod name
func getOrdinal(name string) int {
	ordinal := -1
	sm := statefulPodRegex.FindStringSubmatch(name)
	if len(sm) < 2 {
		return ordinal
	}
	if i, err := strconv.ParseInt(sm[1], 10, 32); err == nil {
		ordinal = int(i)
	}
	return ordinal
}
