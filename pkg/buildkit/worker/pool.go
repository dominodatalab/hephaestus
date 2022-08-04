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
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1beta1 "k8s.io/api/discovery/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	appsv1typed "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryv1beta1typed "k8s.io/client-go/kubernetes/typed/discovery/v1beta1"
	"k8s.io/utils/pointer"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

type Pool interface {
	Get(ctx context.Context, owner string) (workerAddr string, err error)
	Release(ctx context.Context, workerAddr string) error
	Close()
}

var (
	ErrNoUnleasedPods = errors.New("no unleased pods found")

	newUUID          = uuid.NewUUID
	statefulPodRegex = regexp.MustCompile(`^.*-(\d+)$`)
)

const (
	fieldManagerName     = "hephaestus-pod-lease-manager"
	leasedAtAnnotation   = "hephaestus.dominodatalab.com/leased-at"
	leasedByAnnotation   = "hephaestus.dominodatalab.com/leased-by"
	managerIDAnnotation  = "hephaestus.dominodatalab.com/manager-identity"
	expiryTimeAnnotation = "hephaestus.dominodatalab.com/expiry-time"
)

type workerPool struct {
	log logr.Logger

	// system shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// incoming lease requests
	requests RequestQueue

	// worker loop routine
	poolSyncTime    time.Duration
	podMaxIdleTime  time.Duration
	notifyReconcile chan struct{}

	// leasing
	mu                  sync.Mutex
	uuid                string
	namespace           string
	podClient           corev1typed.PodInterface
	endpointSliceClient discoveryv1beta1typed.EndpointSliceInterface

	podListOptions            metav1.ListOptions
	endpointSliceListOptions  metav1.ListOptions
	endpointSliceWatchTimeout int64

	// endpoints discovery
	serviceName string
	servicePort int32

	// statefulset mgmt
	statefulSetName   string
	statefulSetClient appsv1typed.StatefulSetInterface
}

// NewPool creates a new worker pool that can be used to lease buildkit workers for image builds.
func NewPool(ctx context.Context, clientset kubernetes.Interface, conf config.Buildkit, opts ...PoolOption) Pool {
	o := defaultOpts
	for _, fn := range opts {
		o = fn(o)
	}

	pls := labels.SelectorFromSet(conf.PodLabels)
	podListOptions := metav1.ListOptions{LabelSelector: pls.String()}

	esls := labels.SelectorFromSet(map[string]string{"kubernetes.io/service-name": conf.ServiceName})
	endpointSliceListOptions := metav1.ListOptions{LabelSelector: esls.String()}

	ctx, cancel := context.WithCancel(ctx)
	wp := &workerPool{
		ctx:                       ctx,
		cancel:                    cancel,
		log:                       o.Log,
		poolSyncTime:              o.SyncWaitTime,
		podMaxIdleTime:            o.MaxIdleTime,
		endpointSliceWatchTimeout: o.EndpointWatchTimeoutSeconds,
		uuid:                      string(newUUID()),
		requests:                  NewRequestQueue(),
		notifyReconcile:           make(chan struct{}, 1),
		podClient:                 clientset.CoreV1().Pods(conf.Namespace),
		endpointSliceClient:       clientset.DiscoveryV1beta1().EndpointSlices(conf.Namespace),
		podListOptions:            podListOptions,
		endpointSliceListOptions:  endpointSliceListOptions,
		serviceName:               conf.ServiceName,
		servicePort:               conf.DaemonPort,
		statefulSetName:           conf.StatefulSetName,
		statefulSetClient:         clientset.AppsV1().StatefulSets(conf.Namespace),
		namespace:                 conf.Namespace,
	}

	wp.log.Info("Starting worker pod monitor", "syncTime", wp.poolSyncTime.String())
	go func() {
		ticker := time.NewTicker(wp.poolSyncTime)
		defer ticker.Stop()

		for {
			if err := wp.updateWorkers(wp.ctx); err != nil {
				wp.log.Error(err, "Failed to update worker pool")
			}

			select {
			// break out of the select when triggered by notification or tick, this will trigger an update
			case <-wp.notifyReconcile:
				wp.log.Info("Reconciling pool, notify triggered")
			case <-ticker.C:
				wp.log.Info("Reconciling pool, sync triggered")
			case <-wp.ctx.Done():
				wp.log.Info("Shutting down worker pod monitor")
				for wp.requests.Len() > 0 {
					close(wp.requests.Dequeue().result)
				}

				return
			}
		}
	}()

	return wp
}

// Get a lease for a worker in the pool and return a routable address.
//
// Adds "lease"/"manager-identity" metadata and removes "expiry-time".
// The worker will remain leased until the caller provides the address to Release().
func (p *workerPool) Get(ctx context.Context, owner string) (string, error) {
	request := &PodRequest{
		owner:  owner,
		result: make(chan PodRequestResult, 1),
	}

	p.requests.Enqueue(request)
	defer p.requests.Remove(request)

	p.triggerReconcile()

	select {
	case result, ok := <-request.result:
		// check if channel is open before processing
		if ok {
			if result.err != nil {
				return "", result.err
			}

			return result.addr, nil
		}
	case <-ctx.Done():
		// context has been cancelled
		return "", ctx.Err()
	}

	return "", ErrNoUnleasedPods
}

// Release an address back into the worker pool.
//
// Adds "expiry-time" and removes "lease"/"manager-identity" metadata.
// The underlying worker will be terminated after its expiry time has passed.
func (p *workerPool) Release(ctx context.Context, addr string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log.Info("Parsing lease addr", "addr", addr)
	u, err := url.ParseRequestURI(addr)
	if err != nil || u.Host == "" {
		return errors.New("invalid address: must be an absolute URI including scheme")
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

	return p.releasePod(ctx, pod)
}

// Close shuts down the pool by terminating all background routines used to manage requests and garbage collection.
func (p *workerPool) Close() {
	p.cancel()
}

// applies lease metadata to given pod
func (p *workerPool) leasePod(ctx context.Context, pod *corev1.Pod, owner string) error {
	pac, err := corev1ac.ExtractPod(pod, fieldManagerName)
	if err != nil {
		return fmt.Errorf("cannot extract pod config: %w", err)
	}

	pac.WithAnnotations(map[string]string{
		leasedAtAnnotation:  time.Now().Format(time.RFC3339),
		leasedByAnnotation:  owner,
		managerIDAnnotation: p.uuid,
	})
	delete(pac.Annotations, expiryTimeAnnotation)

	p.log.Info("Applying pod metadata changes", "annotations", pac.Annotations)
	if _, err = p.podClient.Apply(ctx, pac, metav1.ApplyOptions{FieldManager: fieldManagerName}); err != nil {
		return fmt.Errorf("cannot update pod metadata: %w", err)
	}

	return nil
}

// removes lease metadata from given pod and adds expiry
func (p *workerPool) releasePod(ctx context.Context, pod *corev1.Pod) error {
	pac, err := corev1ac.ExtractPod(pod, fieldManagerName)
	if err != nil {
		return fmt.Errorf("cannot extract pod config: %w", err)
	}

	pac.WithAnnotations(map[string]string{
		expiryTimeAnnotation: time.Now().Add(p.podMaxIdleTime).Format(time.RFC3339),
	})
	delete(pac.Annotations, leasedAtAnnotation)
	delete(pac.Annotations, leasedByAnnotation)
	delete(pac.Annotations, managerIDAnnotation)

	p.log.Info("Applying pod metadata changes", "annotations", pac.Annotations)
	if _, err = p.podClient.Apply(ctx, pac, metav1.ApplyOptions{FieldManager: fieldManagerName}); err != nil {
		return fmt.Errorf("cannot update pod metadata: %w", err)
	}

	p.triggerReconcile()

	return nil
}

// builds routable url for buildkit pod with protocol and port
func (p *workerPool) buildEndpointURL(ctx context.Context, podName string) (string, error) {
	p.log.Info("Watching endpoints for new address", "podName", podName)

	watchOpts := metav1.ListOptions{
		LabelSelector:  p.endpointSliceListOptions.LabelSelector,
		TimeoutSeconds: &p.endpointSliceWatchTimeout,
	}
	watcher, err := p.endpointSliceClient.Watch(ctx, watchOpts)
	if err != nil {
		return "", fmt.Errorf("failed to watch endpointslices: %w", err)
	}
	defer watcher.Stop()

	var hostname string

	start := time.Now()
	for event := range watcher.ResultChan() {
		endpointSlice := event.Object.(*discoveryv1beta1.EndpointSlice)

		if hostname = p.extractHostname(endpointSlice, podName); hostname != "" {
			break
		}
	}
	p.log.Info("Finished watching endpoints", "podName", podName, "duration", time.Since(start))

	if hostname == "" {
		return "", fmt.Errorf("failed to extract hostname after %d seconds", p.endpointSliceWatchTimeout)
	}

	u, err := url.ParseRequestURI(fmt.Sprintf("tcp://%s:%d", hostname, p.servicePort))
	if err != nil {
		return "", fmt.Errorf("failed to parse endpoint url: %w", err)
	}

	return u.String(), nil
}

// generates internal hostname for pod using an endpoint slice
func (p *workerPool) extractHostname(epSlice *discoveryv1beta1.EndpointSlice, podName string) (hostname string) {
	var portPresent bool
	for _, port := range epSlice.Ports {
		if pointer.Int32Deref(port.Port, 0) == p.servicePort {
			portPresent = true
			break
		}
	}
	if !portPresent {
		return
	}

	for _, endpoint := range epSlice.Endpoints {
		if endpoint.TargetRef.Name != podName {
			continue
		}

		if !pointer.BoolDeref(endpoint.Conditions.Ready, false) {
			break
		}

		if endpoint.Hostname == nil {
			break
		}

		hostname = strings.Join([]string{*endpoint.Hostname, p.serviceName, epSlice.Namespace}, ".")
		p.log.Info("Found eligible endpoint address", "hostname", hostname)

		break
	}

	return
}

// reconcile pods in worker pool
func (p *workerPool) updateWorkers(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log.Info("Querying for pods", "namespace", p.namespace, "opts", p.podListOptions)
	podList, err := p.podClient.List(ctx, p.podListOptions)
	if err != nil {
		return err
	}

	// ensure pod list is sorted ascending
	sort.Slice(podList.Items, func(i, j int) bool {
		return getOrdinal(podList.Items[i].Name) < getOrdinal(podList.Items[j].Name)
	})

	var allPods []string
	var leased []string
	var pending []string
	var removals []string

	// mark pods for removal based on state and lease pods when requests exist
	for _, pod := range podList.Items {
		log := p.log.WithValues("podName", pod.Name)
		allPods = append(allPods, pod.Name)

		if id, ok := pod.Annotations[managerIDAnnotation]; ok && id != p.uuid { // mark unmanaged pods
			log.Info("Eligible for termination, manager id mismatch", "expected", p.uuid, "actual", id)

			removals = append(removals, pod.Name)
		} else if _, hasLease := pod.Annotations[leasedByAnnotation]; hasLease { // mark leased pods
			log.Info("Ineligible for termination, pod is leased")

			leased = append(leased, pod.Name)
		} else if pod.Status.Phase == corev1.PodPending { // mark pending pods
			pending = append(pending, pod.Name)
		} else if pod.Status.Phase == corev1.PodRunning { // dispatch builds/check expiry/check age on running pods
			if req := p.requests.Dequeue(); req != nil {
				log.Info("Found pending pod request, processing")

				if p.processPodRequest(ctx, req, pod) {
					leased = append(leased, pod.Name)
				}
				continue
			}

			if ts, ok := pod.Annotations[expiryTimeAnnotation]; ok {
				expiry, err := time.Parse(time.RFC3339, ts)

				if err != nil {
					log.Info("Cannot parse expiry time, assuming expired", "expiry", expiry)
					removals = append(removals, pod.Name)
				} else if time.Now().After(expiry) {
					log.Info("Eligible for termination, ttl has expired", "expiry", expiry)
					removals = append(removals, pod.Name)
				}
			} else if time.Since(pod.CreationTimestamp.Time) > p.podMaxIdleTime {
				log.Info("Eligible for termination, missing expiry time and pod age older than max idle time")
				removals = append(removals, pod.Name)
			}
		}
	}

	// collect names of pods that might be terminated
	subtractionMap := make(map[string]bool)
	for _, name := range removals {
		subtractionMap[name] = true
	}
	for _, name := range pending {
		subtractionMap[name] = true
	}

	// calculate which pods can be removed based reverse-ordinal position
	var subtractions int
	for idx := range allPods {
		reverseIdx := len(allPods) - 1 - idx

		if subtractionMap[allPods[reverseIdx]] {
			subtractions++
		} else {
			break
		}
	}

	podCount := len(allPods)
	requestCount := p.requests.Len()
	replicas := int32(podCount + requestCount - subtractions)

	p.log.Info("Pod evaluation complete",
		"allPods", allPods,
		"leasedPods", leased,
		"pendingPods", pending,
		"removalPods", removals,
		"podRequests", requestCount,
	)

	p.log.Info("Setting statefulset scale", "replicas", replicas)
	_, err = p.statefulSetClient.UpdateScale(
		ctx,
		p.statefulSetName,
		&autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.statefulSetName,
				Namespace: p.namespace,
			},
			Spec: autoscalingv1.ScaleSpec{Replicas: replicas},
		},
		metav1.UpdateOptions{FieldManager: fieldManagerName},
	)
	return err
}

// attempts to lease a pod, build and endpoint url, and provide a request result
func (p *workerPool) processPodRequest(ctx context.Context, req *PodRequest, pod corev1.Pod) (success bool) {
	log := p.log.WithValues("podName", pod.Name)

	log.Info("Attempting to lease pod")
	if err := p.leasePod(ctx, &pod, req.owner); err != nil {
		log.Error(err, "Failed to lease pod")

		req.result <- PodRequestResult{err: err}
		return
	}

	log.Info("Building endpoint URL")
	addr, err := p.buildEndpointURL(ctx, pod.Name)

	if err != nil {
		log.Error(err, "Failed to build routable URL")

		if rErr := p.releasePod(ctx, &pod); rErr != nil {
			log.Error(rErr, "Failed to release pod")
		}

		req.result <- PodRequestResult{err: err}
		return
	}

	log.Info("Pod successfully leased, passing address to request owner")
	req.result <- PodRequestResult{addr: addr}

	return true
}

// trigger a pool reconciliation
// if there's already a notification pending, continue
func (p *workerPool) triggerReconcile() {
	p.log.Info("Attempting to notify reconciliation")

	select {
	case p.notifyReconcile <- struct{}{}:
		p.log.Info("Notification sent")
	default:
		p.log.Info("Aborting notify, notification already present")
	}
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
