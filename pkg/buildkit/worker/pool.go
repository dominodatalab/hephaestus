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
	"time"

	"github.com/go-logr/logr"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	appsv1typed "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryv1typed "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/utils/ptr"

	"github.com/dominodatalab/hephaestus/pkg/config"
)

type Pool interface {
	Start(ctx context.Context) error
	Get(ctx context.Context, owner string) (workerAddr string, err error)
	Release(ctx context.Context, workerAddr string) error
}

var (
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

var errPoolClosed = errors.New("AutoscalingPool closed")

type AutoscalingPool struct {
	log logr.Logger

	// shutdown
	stopped chan struct{}

	// incoming lease requests
	requests RequestQueue

	// worker loop routine
	poolSyncTime    time.Duration
	podMaxIdleTime  time.Duration
	notifyReconcile chan struct{}

	// leasing
	uuid                string
	namespace           string
	podClient           corev1typed.PodInterface
	nodeClient          corev1typed.NodeInterface
	eventClient         corev1typed.EventInterface
	endpointSliceClient discoveryv1typed.EndpointSliceInterface

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
func NewPool(
	clientset kubernetes.Interface,
	conf config.Buildkit,
	opts ...PoolOption,
) *AutoscalingPool {
	o := defaultOpts
	for _, fn := range opts {
		o = fn(o)
	}

	pls := labels.SelectorFromSet(conf.PodLabels)
	podListOptions := metav1.ListOptions{LabelSelector: pls.String()}

	esls := labels.SelectorFromSet(map[string]string{"kubernetes.io/service-name": conf.ServiceName})
	endpointSliceListOptions := metav1.ListOptions{LabelSelector: esls.String()}

	wp := &AutoscalingPool{
		log:                       o.Log,
		stopped:                   make(chan struct{}),
		poolSyncTime:              o.SyncWaitTime,
		podMaxIdleTime:            o.MaxIdleTime,
		endpointSliceWatchTimeout: o.EndpointWatchTimeoutSeconds,
		uuid:                      string(newUUID()),
		requests:                  NewRequestQueue(),
		notifyReconcile:           make(chan struct{}, 1),
		podClient:                 clientset.CoreV1().Pods(conf.Namespace),
		nodeClient:                clientset.CoreV1().Nodes(),
		eventClient:               clientset.CoreV1().Events(conf.Namespace),
		endpointSliceClient:       clientset.DiscoveryV1().EndpointSlices(conf.Namespace),
		podListOptions:            podListOptions,
		endpointSliceListOptions:  endpointSliceListOptions,
		serviceName:               conf.ServiceName,
		servicePort:               conf.DaemonPort,
		statefulSetName:           conf.StatefulSetName,
		statefulSetClient:         clientset.AppsV1().StatefulSets(conf.Namespace),
		namespace:                 conf.Namespace,
	}
	return wp
}

func (p *AutoscalingPool) Start(ctx context.Context) error {
	p.log.Info("Starting worker pod monitor", "syncTime", p.poolSyncTime.String())

	ticker := time.NewTicker(p.poolSyncTime)

	defer func() {
		ticker.Stop()
		p.log.Info("Shutting down worker pod monitor")
		for p.requests.Len() > 0 {
			close(p.requests.Dequeue().result)
		}
		close(p.stopped)
	}()

	for {
		if err := p.reconcileWorkers(ctx); err != nil {
			p.log.Error(err, "Failed to update worker pool")
		}

		select {
		// break out of the select when triggered by notification or tick, this will trigger an update
		case <-p.notifyReconcile:
			p.log.Info("Reconciling pool, notify triggered")
		case <-ticker.C:
			p.log.Info("Reconciling pool, sync triggered")
		case <-ctx.Done():
			return nil
		}
	}
}

// Get a lease for a worker in the pool and return a routable address.
//
// Adds "lease"/"manager-identity" metadata and removes "expiry-time".
// The worker will remain leased until the caller provides the address to Release().
func (p *AutoscalingPool) Get(ctx context.Context, owner string) (string, error) {
	request := &PodRequest{
		owner:  owner,
		result: make(chan PodRequestResult, 1),
	}

	p.log.Info("Enqueuing new pod request")
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
	case <-p.stopped:
	}

	return "", errPoolClosed
}

// Release an address back into the worker pool.
//
// Adds "expiry-time" and removes "lease"/"manager-identity" metadata.
// The underlying worker will be terminated after its expiry time has passed.
func (p *AutoscalingPool) Release(ctx context.Context, addr string) error {
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

	return p.releasePod(ctx, *pod)
}

// applies lease metadata to given pod
func (p *AutoscalingPool) leasePod(ctx context.Context, pod corev1.Pod, owner string) error {
	pac, err := corev1ac.ExtractPod(&pod, fieldManagerName)
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
func (p *AutoscalingPool) releasePod(ctx context.Context, pod corev1.Pod) error {
	pac, err := corev1ac.ExtractPod(&pod, fieldManagerName)
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
func (p *AutoscalingPool) buildEndpointURL(ctx context.Context, pod corev1.Pod) (string, error) {
	p.log.Info("Watching endpoints for new pod address", "podName", pod.Name)

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
		endpointSlice := event.Object.(*discoveryv1.EndpointSlice)

		if hostname = p.extractHostname(endpointSlice, pod.Name); hostname != "" {
			break
		}
	}

	if end := time.Since(start); end < time.Duration(p.endpointSliceWatchTimeout)*time.Second {
		p.log.Info("Finished watching endpoints", "podName", pod.Name, "duration", end)
	} else {
		p.log.Info("Endpoint watch timed out")
	}

	if hostname == "" {
		p.diagnoseFailure(ctx, pod)
		return "", fmt.Errorf("failed to extract hostname after %d seconds", p.endpointSliceWatchTimeout)
	}

	u, err := url.ParseRequestURI(fmt.Sprintf("tcp://%s:%d", hostname, p.servicePort))
	if err != nil {
		return "", fmt.Errorf("failed to parse endpoint url: %w", err)
	}

	return u.String(), nil
}

// generates internal hostname for pod using an endpoint slice
func (p *AutoscalingPool) extractHostname(epSlice *discoveryv1.EndpointSlice, podName string) (hostname string) {
	var portPresent bool
	for _, port := range epSlice.Ports {
		if ptr.Deref(port.Port, 0) == p.servicePort {
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

		if !ptr.Deref(endpoint.Conditions.Ready, false) {
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
func (p *AutoscalingPool) reconcileWorkers(ctx context.Context) error {
	p.log.Info("Querying for available buildkit pods", "namespace", p.namespace, "opts", p.podListOptions)
	podList, err := p.podClient.List(ctx, p.podListOptions)
	if err != nil {
		return err
	}

	// ensure pod list is sorted ascending
	sort.Slice(podList.Items, func(i, j int) bool {
		return getOrdinal(podList.Items[i].Name) < getOrdinal(podList.Items[j].Name)
	})

	arbiter := NewScaleArbiter(p.log, p.podClient, p.podMaxIdleTime)

	for _, pod := range podList.Items {
		p.log.Info("Evaluating pod metadata and status", "podName", pod.Name)
		arbiter.EvaluatePod(ctx, p.uuid, pod)
	}
	for _, observation := range arbiter.LeasablePods() {
		req := p.requests.Dequeue()
		if req == nil {
			break
		}

		p.log.Info("Processing dequeued pod request with operational pod")
		if p.processPodRequest(ctx, req, observation.Pod) {
			observation.MarkLeased()
		}
	}

	replicas := arbiter.DetermineReplicas(p.requests.Len())

	p.log.Info("Using statefulset scale", "replicas", replicas)
	_, err = p.statefulSetClient.UpdateScale(
		ctx,
		p.statefulSetName,
		&autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.statefulSetName,
				Namespace: p.namespace,
			},
			Spec: autoscalingv1.ScaleSpec{Replicas: int32(replicas)},
		},
		metav1.UpdateOptions{FieldManager: fieldManagerName},
	)
	return err
}

// attempts to lease a pod, build and endpoint url, and provide a request result
func (p *AutoscalingPool) processPodRequest(ctx context.Context, req *PodRequest, pod corev1.Pod) (success bool) {
	log := p.log.WithValues("podName", pod.Name)

	log.Info("Attempting to lease pod")
	if err := p.leasePod(ctx, pod, req.owner); err != nil {
		log.Error(err, "Failed to lease pod")

		req.result <- PodRequestResult{err: err}
		return
	}

	log.Info("Building endpoint URL")
	addr, err := p.buildEndpointURL(ctx, pod)
	if err != nil {
		log.Error(err, "Failed to build routable URL")

		if rErr := p.releasePod(ctx, pod); rErr != nil {
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
func (p *AutoscalingPool) triggerReconcile() {
	p.log.Info("Attempting to notify reconciliation")

	select {
	case p.notifyReconcile <- struct{}{}:
		p.log.Info("Reconciliation notification sent")
	default:
		p.log.Info("Aborting notify, notification already present")
	}
}

// diagnose elements that could lead to a failure
func (p *AutoscalingPool) diagnoseFailure(ctx context.Context, pod corev1.Pod) {
	log := p.log.WithName("diagnosis").WithValues("podName", pod.Name)

	log.Info("Beginning failure diagnosis")
	p.diagnosePod(ctx, pod.Name)
	p.diagnoseEvents(ctx, pod)
	p.diagnoseEndpointSlices(ctx, pod.Name)
	log.Info("Failure diagnosis completed")
}

// diagnose issues with endpoint slices
func (p *AutoscalingPool) diagnoseEndpointSlices(ctx context.Context, podName string) {
	log := p.log.WithName("diagnosis").WithName("endpointslice").WithValues("podName", podName)

	listOpts := metav1.ListOptions{LabelSelector: p.endpointSliceListOptions.LabelSelector}
	endpointSliceList, err := p.endpointSliceClient.List(ctx, listOpts)
	if err != nil {
		log.Error(err, "Failed to list endpoint slices during diagnosis")
		return
	}
	log.Info("Found endpoint slices", "list", endpointSliceList.Items)

	for _, endpointSlice := range endpointSliceList.Items {
		for _, endpoint := range endpointSlice.Endpoints {
			if endpoint.TargetRef.Name == podName {
				log.Info("Found endpoint for pod", "endpoint", endpoint)

				if !ptr.Deref(endpoint.Conditions.Ready, false) {
					log.Info("Endpoint IS NOT ready")
				}
				if !ptr.Deref(endpoint.Conditions.Serving, false) {
					log.Info("Endpoint IS NOT serving")
				}
				if ptr.Deref(endpoint.Conditions.Terminating, false) {
					log.Info("Endpoint IS terminating")
				}

				return
			}
		}
	}
	log.Info("Unable to find endpoint for pod")
}

// diagnose issues with pods
func (p *AutoscalingPool) diagnosePod(ctx context.Context, podName string) {
	log := p.log.WithName("diagnosis").WithName("pod").WithValues("podName", podName)

	pod, err := p.podClient.Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get pod during diagnosis")
		}

		log.Info("Pod not found")
		return
	}
	log.Info("Pod details", "spec", pod.Spec, "status", pod.Status)

	if pod.Status.Phase != corev1.PodRunning {
		log.Info("Pod is NOT running", "phase", pod.Status.Phase)
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Status == corev1.ConditionTrue {
			continue
		}

		var message string
		switch condition.Type {
		case corev1.PodScheduled:
			message = "Pod is NOT scheduled"
		case corev1.PodInitialized:
			message = "Pod is NOT initialized"
		case corev1.ContainersReady:
			message = "All pod containers are NOT ready"
		case corev1.PodReady:
			message = "Pod is NOT ready to serve requests"
		default:
			message = "Unexpected pod condition type"
		}

		log.Info(
			message,
			"reason", condition.Reason,
			"message", condition.Message,
			"lastTransitionTime", condition.LastTransitionTime,
		)
	}

	node, err := p.nodeClient.Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to node during diagnosis")
		}

		log.Info("Node NOT found")
		return
	}
	log.Info("Node details", "conditions", node.Status.Conditions)
}

// inspect events related to pod
func (p *AutoscalingPool) diagnoseEvents(ctx context.Context, pod corev1.Pod) {
	log := p.log.WithName("diagnosis").WithName("event").WithValues("podName", pod.Name)

	listOpts := metav1.ListOptions{
		FieldSelector: fields.Set{
			"involvedObject.kind":            "Pod",
			"involvedObject.name":            pod.Name,
			"involvedObject.resourceVersion": pod.ResourceVersion,
		}.String(),
	}

	eventList, err := p.eventClient.List(ctx, listOpts)
	if err != nil {
		log.Error(err, "Failed to list events during diagnosis")
	}

	for _, event := range eventList.Items {
		log.Info(
			"Event found",
			"firstSeen", event.FirstTimestamp,
			"lastSeen", event.LastTimestamp,
			"type", event.Type,
			"reason", event.Reason,
			"subject", event.InvolvedObject.FieldPath,
			"source", event.Source.String(),
			"message", event.Message,
			"count", event.Count,
		)
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
