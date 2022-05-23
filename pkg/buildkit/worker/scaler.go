package worker

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	"k8s.io/client-go/kubernetes"
	appsv1typed "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/utils/pointer"
)

type Scaler interface {
	Scale(ctx context.Context, count int32) error
}

type ScalerOpt struct {
	Log                 logr.Logger
	Name                string
	Namespace           string
	Clientset           kubernetes.Interface
	WatchTimeoutSeconds int64
}

type statefulSetScaler struct {
	mu sync.Mutex

	log                 logr.Logger
	name                string
	client              appsv1typed.StatefulSetInterface
	watchTimeoutSeconds *int64
}

func NewStatefulSetScaler(opt ScalerOpt) Scaler {
	return &statefulSetScaler{
		log:                 opt.Log,
		name:                opt.Name,
		client:              opt.Clientset.AppsV1().StatefulSets(opt.Namespace),
		watchTimeoutSeconds: pointer.Int64(opt.WatchTimeoutSeconds),
	}
}

func (s *statefulSetScaler) Scale(ctx context.Context, count int32) error {
	sts, err := s.patchReplicas(ctx, count)
	if err != nil {
		return err
	}
	replicas := *sts.Spec.Replicas

	listOpts := metav1.ListOptions{
		LabelSelector:  labels.FormatLabels(sts.Labels),
		TimeoutSeconds: s.watchTimeoutSeconds,
	}

	s.log.V(1).Info("Starting statefulset watch", "opts", listOpts)
	watcher, err := s.client.Watch(ctx, listOpts)
	if err != nil {
		return fmt.Errorf("cannot watch statefulset: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		target := event.Object.(*appsv1.StatefulSet)
		if target.Status.ReadyReplicas >= replicas {
			s.log.V(1).Info("Stateful replicas ready, stopping watch")
			break
		} else {
			s.log.V(1).Info("Stateful replicas not ready, still watching", "expected", replicas, "actual", target.Status.ReadyReplicas)
		}
	}

	return nil
}

func (s *statefulSetScaler) patchReplicas(ctx context.Context, count int32) (*appsv1.StatefulSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.log.V(1).Info("Querying for statefulset", "name", s.name)
	sts, err := s.client.Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("cannot get statefulset: %w", err)
	}

	current := pointer.Int32Deref(sts.Spec.Replicas, 0)
	desired := current + count
	s.log.V(1).Info("Replica count", "current", current, "desired", desired)

	sac, err := appsv1ac.ExtractStatefulSet(sts, fieldManagerName)
	if err != nil {
		return nil, err
	}
	sac.WithSpec(appsv1ac.StatefulSetSpec().WithReplicas(desired))

	s.log.Info("Applying statefulset replicas change", "name", sts.Name, "spec", sac.Spec)
	sts, err = s.client.Apply(ctx, sac, metav1.ApplyOptions{FieldManager: fieldManagerName, Force: true})
	if err != nil {
		return nil, fmt.Errorf("cannot patch statefulset: %w", err)
	}

	return sts, nil
}
