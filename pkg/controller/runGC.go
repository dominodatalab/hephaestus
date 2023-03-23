package controller

import (
	"context"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/clientset"
	"github.com/dominodatalab/hephaestus/pkg/kubernetes"
	"github.com/go-logr/logr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sort"
)

type ImageBuildGC struct {
	maxIBRetention int
	hephClient     clientset.Interface
}

func NewImageBuildGC(maxIBRetention int, log logr.Logger) (
	*ImageBuildGC, error) {

	log.Info("Initializing Kubernetes V1 CRD client")

	config, err := kubernetes.RestConfig()
	if err != nil {
		return nil, err
	}

	//client, err := apixv1client.NewForConfig(config)
	//if err != nil {
	//	return nil, err
	//}
	hephClient, _ := clientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &ImageBuildGC{
		maxIBRetention: maxIBRetention,
		hephClient:     hephClient,
	}, nil
}

func (gc *ImageBuildGC) CleanUpIBs(ctx context.Context, log logr.Logger) {
	crdList, err := gc.hephClient.HephaestusV1().ImageBuilds("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Info("Unable to access a list of IBs, not starting IB clean up", "error", err)
		return
	}

	listLen := len(crdList.Items)
	if listLen == 0 {
		log.V(1).Info("No build resources found, aborting")
		return
	}

	var builds []hephv1.ImageBuild
	for _, ib := range crdList.Items {
		state := ib.Status.Phase
		if state == hephv1.PhaseFailed || state == hephv1.PhaseSucceeded {
			builds = append(builds, ib)
		}
	}

	if len(builds) <= gc.maxIBRetention {
		log.Info("Total resources are less than or equal to retention limit, aborting",
			"resourceCount", len(builds), "retentionCount", gc.maxIBRetention)
		return
	}
	log.Info("Total resources eligible for deletion", "count", len(builds))
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].CreationTimestamp.Before(&builds[j].CreationTimestamp)
	})
	deletePolicy := metav1.DeletePropagationForeground
	for _, build := range builds[:len(builds)-gc.maxIBRetention] {
		if err := gc.hephClient.HephaestusV1().ImageBuilds("default").Delete(ctx, build.Name,
			metav1.DeleteOptions{PropagationPolicy: &deletePolicy}); err != nil {
			log.Info("Failed to delete build", "name", build.Name, "namespace", build.Namespace, "error", err)
		}
		log.Info("Deleted build", "name", build.Name, "namespace", build.Namespace)
	}
	log.Info("Cleanup complete")
}

func RunGC(enabled bool, maxIBRetention int) error {
	log := ctrlzap.New(
		ctrlzap.UseDevMode(true),
		ctrlzap.Encoder(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())),
	)
	log = log.WithName("GC")

	if !enabled {
		log.Info("Auto IB clean up disabled, please manually clean up your image builds.")
		return nil
	}
	ctx := context.Background()

	gc, err := NewImageBuildGC(maxIBRetention, log)
	if err != nil {
		log.Info("Error setting up GC", "error", err)
		return err
	}

	log.Info("Launching Image Build Clean up", "maxIBRetention", gc.maxIBRetention)
	gc.CleanUpIBs(ctx, log)
	return nil
}
