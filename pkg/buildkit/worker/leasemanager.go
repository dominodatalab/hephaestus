package worker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
)

type LeaseManager interface {
	Lease(ctx context.Context) (string, error)
	Release(ctx context.Context, addr string) error
}

type LeaseManagerOpt struct {
	Log            logr.Logger
	Clienset       kubernetes.Interface
	Namespace      string
	PodLabels      map[string]string
	ServiceName    string
	ServicePort    int32
	WorkloadScaler Scaler
	PodSyncTime    time.Duration
	PodMaxIdleTime time.Duration
}

var errNoUnleasedPods = errors.New("no unleased pods found")

const (
	fieldManagerName     = "hephaestus-pod-lease-manager"
	leasedAnnotation     = "hephaestus.dominodatalab.com/leased"
	managerIDAnnotation  = "hephaestus.dominodatalab.com/manager-identity"
	expiryTimeAnnotation = "hephaestus.dominodatalab.com/expiry-time"
)

type podTransport struct {
	pod *corev1.Pod
	err error
}

type podLeaseManager struct {
	mu sync.Mutex

	log            logr.Logger
	uuid           string
	ptch           chan podTransport
	podSyncTime    time.Duration
	podMaxIdleTime time.Duration

	serviceName string
	servicePort int32
	namespace   string

	scaler          Scaler
	podClient       corev1typed.PodInterface
	endpointsClient corev1typed.EndpointsInterface
	podListOptions  metav1.ListOptions
}

func NewPodLeaseManager(ctx context.Context, opt LeaseManagerOpt) LeaseManager {
	fs := fields.SelectorFromSet(map[string]string{"status.phase": "Running"})
	ls := labels.SelectorFromSet(opt.PodLabels)
	podListOptions := metav1.ListOptions{
		LabelSelector: ls.String(),
		FieldSelector: fs.String(),
	}

	manager := &podLeaseManager{
		log:             opt.Log,
		ptch:            make(chan podTransport),
		uuid:            string(uuid.NewUUID()),
		podSyncTime:     opt.PodSyncTime,
		podMaxIdleTime:  opt.PodMaxIdleTime,
		podClient:       opt.Clienset.CoreV1().Pods(opt.Namespace),
		endpointsClient: opt.Clienset.CoreV1().Endpoints(opt.Namespace),
		podListOptions:  podListOptions,
		serviceName:     opt.ServiceName,
		servicePort:     opt.ServicePort,
		namespace:       opt.Namespace,
		scaler:          opt.WorkloadScaler,
	}
	go manager.monitorPods(ctx)

	return manager
}

func (m *podLeaseManager) Lease(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pod, err := m.nextAvailablePod(ctx)
	if err != nil {
		return "", err
	}

	addr, err := m.buildURL(ctx, pod.Name)
	if err != nil {
		return "", err
	}

	pac, err := corev1ac.ExtractPod(pod, fieldManagerName)
	if err != nil {
		return "", fmt.Errorf("cannot extract pod config: %w", err)
	}

	pac.WithAnnotations(map[string]string{
		leasedAnnotation:    "true",
		managerIDAnnotation: m.uuid,
	})
	delete(pac.Annotations, expiryTimeAnnotation)

	m.log.Info("Applying pod metadata changes", "annotations", pac.Annotations)
	if _, err = m.podClient.Apply(ctx, pac, metav1.ApplyOptions{FieldManager: fieldManagerName}); err != nil {
		return "", fmt.Errorf("cannot update pod metadata: %w", err)
	}

	return addr, nil
}

func (m *podLeaseManager) Release(ctx context.Context, addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Info("Parsing lease addr", "addr", addr)
	u, err := url.ParseRequestURI(addr)
	if err != nil {
		return fmt.Errorf("failed to parse lease addr: %w", err)
	}
	podName := strings.Split(u.Host, ".")[0]

	m.log.Info("Querying for pod", "name", podName, "namespace", m.namespace)
	pod, err := m.podClient.Get(ctx, podName, metav1.GetOptions{})
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
		expiryTimeAnnotation: time.Now().Format(time.RFC3339),
	})
	delete(pac.Annotations, leasedAnnotation)
	delete(pac.Annotations, managerIDAnnotation)

	m.log.Info("Applying pod metadata changes", "annotations", pac.Annotations)
	if pod, err = m.podClient.Apply(ctx, pac, metav1.ApplyOptions{FieldManager: fieldManagerName}); err != nil {
		return fmt.Errorf("cannot update pod metadata: %w", err)
	}

	go func() {
		m.ptch <- podTransport{pod: pod, err: nil}
	}()

	return nil
}

func (m *podLeaseManager) nextAvailablePod(ctx context.Context) (*corev1.Pod, error) {
	select {
	case pt := <-m.ptch:
		return pt.pod, pt.err
	default:
		go func(ctx context.Context) {
			pod, err := m.getUnleasedOrScale(ctx)
			m.ptch <- podTransport{pod: pod, err: err}
		}(ctx)
	}

	res := <-m.ptch
	return res.pod, res.err
}

func (m *podLeaseManager) getUnleasedOrScale(ctx context.Context) (*corev1.Pod, error) {
	m.log.Info("Checking for unleased pods")
	pod, err := m.getUnleasedPod(ctx)
	if err == nil {
		return pod, err
	}

	if !errors.Is(err, errNoUnleasedPods) {
		return nil, fmt.Errorf("unexpected leasing error: %w", err)
	}

	m.log.Info("Scaling cluster, no unleased pods found")
	if err = m.scaler.Scale(ctx, 1); err != nil {
		return nil, err
	}

	m.log.Info("No unleased pods found, scaling workload")
	if pod, err = m.getUnleasedPod(ctx); err != nil {
		return nil, fmt.Errorf("unable to acquire lease after scaling: %w", err)
	}

	return pod, nil
}

func (m *podLeaseManager) getUnleasedPod(ctx context.Context) (*corev1.Pod, error) {
	m.log.Info("Querying for pods", "namespace", m.namespace, "opts", m.podListOptions)
	list, err := m.podClient.List(ctx, m.podListOptions)
	if err != nil {
		return nil, err
	}

	for _, pod := range list.Items {
		if _, ok := pod.Annotations[leasedAnnotation]; !ok {
			m.log.Info("Found unleased pod", "name", pod.Name, "namespace", pod.Namespace)
			return &pod, nil
		}
	}

	m.log.Info("No unleased pods found")
	return nil, errNoUnleasedPods
}

func (m *podLeaseManager) buildURL(ctx context.Context, podName string) (string, error) {
	m.log.Info("Querying for endpoints", "name", m.serviceName, "namespace", m.namespace)
	endpoints, err := m.endpointsClient.Get(ctx, m.serviceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var host string

addressScan:
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.TargetRef.Name == podName {
				for _, port := range subset.Ports {
					if port.Port == m.servicePort {
						host = strings.Join([]string{address.Hostname, endpoints.Name, endpoints.Namespace}, ".")
						m.log.Info("Found eligible endpoint address", "host", host)

						break addressScan
					}
				}
			}
		}
	}

	if host == "" {
		return "", fmt.Errorf("endpoints %q does not expose pod %s on port %d", endpoints.Name, podName, 1234)
	}

	u, err := url.ParseRequestURI(fmt.Sprintf("tcp://%s:%d", host, m.servicePort))
	if err != nil {
		return "", fmt.Errorf("failed to parse endpoint url: %w", err)
	}

	return u.String(), nil
}

func (m *podLeaseManager) monitorPods(ctx context.Context) {
	m.log.Info("Starting worker pod monitor", "syncTime", m.podSyncTime.String())
	ticker := time.NewTicker(m.podSyncTime)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log.Info("Shutting down worker pod monitor")
			return
		case <-ticker.C:
			m.log.Info("Checking worker pool")
			if err := m.terminateUnleasedPods(ctx); err != nil {
				m.log.Error(err, "Failed to clean worker pool")
			}
			m.log.Info("Worker pool check complete")
		}
	}
}

func (m *podLeaseManager) terminateUnleasedPods(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Info("Querying for pods", "namespace", m.namespace, "opts", m.podListOptions)
	list, err := m.podClient.List(ctx, m.podListOptions)
	if err != nil {
		return err
	}

	var removals []string
	for idx := len(list.Items) - 1; idx >= 0; idx-- {
		pod := list.Items[idx]
		log := m.log.WithValues("podName", pod.Name)

		if id, ok := pod.Annotations[managerIDAnnotation]; ok && id != m.uuid {
			log.Info("Eligible for termination, manager id mismatch", "expected", m.uuid, "actual", id)
			removals = append(removals, pod.Name)

			continue
		}

		if _, leased := pod.Annotations[leasedAnnotation]; leased {
			break
		}

		ts, ok := pod.Annotations[expiryTimeAnnotation]
		if !ok {
			m.log.Info("Eligible for termination, missing expiry time")
			removals = append(removals, pod.Name)

			continue
		}

		expiry, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return fmt.Errorf("failed to parse pod expiry time: %w", err)
		}

		if time.Since(expiry) > m.podMaxIdleTime {
			m.log.Info("Eligible for termination, ttl has expired", "expiry", expiry)
			removals = append(removals, pod.Name)
		}
	}
	count := -int32(len(removals))

	if count == 0 {
		m.log.Info("No pods eligible for termination")
		return nil
	}

	m.log.Info("Attempting to terminate pods via scale", "names", removals)
	return m.scaler.Scale(ctx, count)
}
