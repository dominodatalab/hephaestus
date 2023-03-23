package controller

import (
	"context"
	"sort"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ImageBuildGC struct {
	maxIBRetention int
	Client         client.Client
	ctx            context.Context
}

// blocks until all resources belonging to a ContainerImageBuild have been deleted
const gcDeleteOpt = client.PropagationPolicy(metav1.DeletePropagationForeground)

func NewImageBuildGC(ctx context.Context, cfg config.Controller, maxIBRetention int, log logr.Logger) (
	*ImageBuildGC, error) {
	mgr, err := createManager(log, cfg.Manager)

	if err != nil {
		return nil, err
	}

	return &ImageBuildGC{
		maxIBRetention: maxIBRetention,
		Client:         mgr.GetClient(),
		ctx:            ctx,
	}, nil
}

func (gc *ImageBuildGC) CleanUpIBs(log logr.Logger) {
	ibList := &hephv1.ImageBuildList{}
	if err := gc.Client.List(gc.ctx, ibList); err != nil {
		log.Info("Unable to access a list of IBs, not starting IB clean up", "error", err)
		return
	}

	listLen := len(ibList.Items)
	if listLen == 0 {
		log.V(1).Info("No build resources found, aborting")
		return
	}

	var builds []hephv1.ImageBuild
	for _, ib := range ibList.Items {
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

	for i, build := range builds[:len(builds)-gc.maxIBRetention] {
		if err := gc.Client.Delete(gc.ctx, &builds[i], gcDeleteOpt); err != nil {
			log.Info("Failed to delete build", "name", build.Name, "namespace", build.Namespace, "error", err)
		}
		log.Info("Deleted build", "name", build.Name, "namespace", build.Namespace)
	}
	log.Info("Cleanup complete")
	return
}

func RunGC(enabled bool, maxIBRetention int, cfg config.Controller) error {
	log := ctrl.Log.WithName("GC")

	if !enabled {
		log.Info("Auto IB clean up disabled, please manually clean up your image builds.")
		return nil
	}
	ctx := context.Background()

	gc, err := NewImageBuildGC(ctx, cfg, maxIBRetention, log)
	if err != nil {
		log.Info("Error setting up GC", "error", err)
		return err
	}

	log.Info("Launching Image Build Clean up", "maxIBRetention", gc.maxIBRetention)
	gc.CleanUpIBs(log)
	return nil
}
