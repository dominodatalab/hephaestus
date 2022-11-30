package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"
)

// BuilderState is an observed buildkit pod state.
type BuilderState int

const (
	// BuilderStateUnmanaged indicates a pod's manager ID is not current.
	BuilderStateUnmanaged BuilderState = iota
	// BuilderStateLeased indicates a pod has been leased for building.
	BuilderStateLeased
	// BuilderStatePending indicates a pod is not fully-operational.
	BuilderStatePending
	// BuilderStatePendingExpired indicates a pod has been in a pending state longer than its expiry.
	BuilderStatePendingExpired
	// BuilderStateStarting indicates a pod is running but not ready for work.
	BuilderStateStarting
	// BuilderStateStartingExpired indicates a pod has been in a starting state longer than its expiry.
	BuilderStateStartingExpired
	// BuilderStateOperational indicates a pod is ready to build.
	BuilderStateOperational
	// BuilderStateOperationalExpired indicates an operational pod's TTL has expired.
	BuilderStateOperationalExpired
	// BuilderStateOperationalInvalidExpiry indicates an operational pod has bad expiry data in its annotations.
	BuilderStateOperationalInvalidExpiry
	// BuilderStateUnusable indicates a pod has an unknown phase or set of conditions.
	BuilderStateUnusable
)

// String representation of the builder state.
func (bs BuilderState) String() string {
	return [...]string{
		"Unmanaged",
		"Leased",
		"Pending",
		"PendingExpired",
		"Starting",
		"StartingExpired",
		"Operational",
		"OperationalExpired",
		"OperationalInvalidExpiry",
		"Unusable",
	}[bs]
}

// PodObservation records the builder state for a single pod.
type PodObservation struct {
	Pod   corev1.Pod
	State BuilderState
}

// MarkLeased should be invoked whenever the caller leases a pod that has been previously evaluated.
func (m *PodObservation) MarkLeased() {
	m.State = BuilderStateLeased
}

// String renders the name and state of the observed pod.
func (m *PodObservation) String() string {
	return fmt.Sprintf("%v - %v", m.Pod.Name, m.State)
}

// ScaleArbiter can be used to determine the proper number of replicas for a
// buildkit statefulset based on the number of build requests and existing pods.
type ScaleArbiter struct {
	log          logr.Logger
	podClient    corev1typed.PodInterface
	podExpiry    time.Duration
	observations []*PodObservation
}

// NewScaleArbiter initializes
func NewScaleArbiter(log logr.Logger, podClient corev1typed.PodInterface, podExpiry time.Duration) *ScaleArbiter {
	return &ScaleArbiter{
		log:       log,
		podClient: podClient,
		podExpiry: podExpiry,
	}
}

// EvaluatePod builder state and store the observation for use when determining the scale replicas.
func (a *ScaleArbiter) EvaluatePod(ctx context.Context, uuid string, pod corev1.Pod) {
	log := a.log.WithValues("podName", pod.Name)

	// mark pods when their manager ID is different from the current one
	if id, ok := pod.Annotations[managerIDAnnotation]; ok && id != uuid {
		log.Info("Eligible for termination, manager id mismatch", "expected", uuid, "actual", id)
		a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStateUnmanaged})

		return
	}

	// mark leased pods to safeguard them from multi-leasing and termination
	if _, hasLease := pod.Annotations[leasedByAnnotation]; hasLease {
		log.Info("Ineligible for termination, pod is leased")
		a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStateLeased})

		return
	}

	// mark pending pods and observe if their ttl has expired
	if pod.Status.Phase == corev1.PodPending {
		if time.Since(pod.CreationTimestamp.Time) < a.podExpiry {
			log.Info("Ineligible for termination, pending pod is not old enough")
			a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStatePending})
		} else {
			log.Info("Eligible for termination, pending pod is older than max idle time")
			a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStatePendingExpired})
		}

		return
	}

	// mark operational pods to service build requests and observe if their ttl is invalid or has expired
	if a.isOperationalPod(ctx, log, pod.Name) {
		log.Info("Pod is operational")
		pm := &PodObservation{Pod: pod, State: BuilderStateOperational}

		if ts, ok := pod.Annotations[expiryTimeAnnotation]; ok {
			expiry, err := time.Parse(time.RFC3339, ts)

			if err != nil {
				log.Info("Cannot parse expiry time, assuming expired", "expiry", expiry)
				pm.State = BuilderStateOperationalInvalidExpiry
			} else if time.Now().After(expiry) {
				log.Info("Eligible for termination, ttl has expired", "expiry", expiry)
				pm.State = BuilderStateOperationalExpired
			}
		} else if time.Since(pod.CreationTimestamp.Time) > a.podExpiry {
			log.Info("Eligible for termination, missing expiry time and pod age older than max idle time")
			pm.State = BuilderStateOperationalExpired
		}
		a.observations = append(a.observations, pm)

		return
	}

	// mark pods that are in the process of starting up and observe if their ttl has expired
	if pod.Status.Phase == corev1.PodRunning {
		if time.Since(pod.CreationTimestamp.Time) < a.podExpiry {
			log.Info("Ineligible for termination, starting pod is not old enough")
			a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStateStarting})
		} else {
			log.Info("Eligible for termination, starting pod is older than max idle time")
			a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStateStartingExpired})
		}

		return
	}

	// mark abnormal pods as unusable
	log.Info(
		"Eligible for termination, unknown phase or incomplete startup detected",
		"phase", pod.Status.Phase,
		"conditions", pod.Status.Conditions,
		"containerStatuses", pod.Status.ContainerStatuses,
	)
	a.observations = append(a.observations, &PodObservation{Pod: pod, State: BuilderStateUnusable})
}

// LeasablePods returns a list of pods that are ready to build images.
func (a *ScaleArbiter) LeasablePods() (observations []*PodObservation) {
	for _, o := range a.observations {
		switch o.State {
		case BuilderStateOperational, BuilderStateOperationalExpired, BuilderStateOperationalInvalidExpiry:
			observations = append(observations, o)
		}
	}

	return
}

// DetermineReplicas calculates the number of buildkit replicas required to service the incoming requests.
func (a *ScaleArbiter) DetermineReplicas(requests int) int {
	count := 0

	var output []string
	for idx, observation := range a.observations {
		output = append(output, observation.String())

		switch observation.State {
		case BuilderStateLeased:
			count = idx + 1
		case BuilderStatePending, BuilderStateStarting, BuilderStateOperational:
			count = idx + 1
			if requests > 0 {
				requests--
			}
		}
	}

	desiredReplicas := 0

	// count is the absolute minimum number of replicas we can set
	// all current replicas >= count are invalid or expired
	if len(a.observations)-count > 0 {
		// we prioritize removal of invalid pods over servicing of build requests
		// the build request will be serviced on the next reconciliation loop
		desiredReplicas = count
	} else {
		desiredReplicas = count + requests
	}

	a.log.Info(
		"Pod scale determination complete",
		"requests", requests,
		"podObservations", output,
		"suggestedReplicas", desiredReplicas,
	)

	return desiredReplicas
}

// ensure pod is operational by checking its phase and conditions
func (a *ScaleArbiter) isOperationalPod(ctx context.Context, log logr.Logger, podName string) (verdict bool) {
	// fetch the latest version of the pod
	pod, err := a.podClient.Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		log.Error(err, "Failed to check if pod is operational")
		return
	}

	// this does not mean the pod is usable but is a good sanity check
	if pod.Status.Phase != corev1.PodRunning {
		return
	}

	// assert the following:
	// 	- pod has been scheduled on a node
	// 	- all init containers have completed successfully
	// 	- all containers in the pod are ready
	// 	- the pod is able to serve requests and should be added to the load balancing pools of all matching Services
	var scheduled, initialized, containersReady, podReady bool
	for _, condition := range pod.Status.Conditions {
		switch condition.Type {
		case corev1.PodScheduled:
			scheduled = condition.Status == corev1.ConditionTrue
		case corev1.PodInitialized:
			initialized = condition.Status == corev1.ConditionTrue
		case corev1.ContainersReady:
			containersReady = condition.Status == corev1.ConditionTrue
		case corev1.PodReady:
			podReady = condition.Status == corev1.ConditionTrue
		}
	}

	// lastly, the status fields are not updated when a pod is terminated, so we have to check the metadata to determine
	// if the pod was deleted by a scale-down event and the process is taking longer than expected to exit
	notDeleted := pod.DeletionTimestamp == nil

	return scheduled && initialized && containersReady && podReady && notDeleted
}
