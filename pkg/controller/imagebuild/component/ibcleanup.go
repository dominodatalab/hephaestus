package component

import (
	"strconv"
	"time"

	"github.com/dominodatalab/controller-util/core"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

type IBCleanUpComponent struct {
	RetentionCount    int
	IBCleanUpInterval int
}

func IBCleanUp(retCount int, cleanupInterval int) *IBCleanUpComponent {
	return &IBCleanUpComponent{
		retCount,
		cleanupInterval,
	}
}

func (c *IBCleanUpComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	cleanUpDisabled := make(chan bool)
	log := ctx.Log
	ticker := time.NewTicker(time.Duration(c.IBCleanUpInterval) * time.Minute)

	if c.IBCleanUpInterval > 0 {
		defer func() {
			log.Info("Shutting down IB cleanup routine")
			cleanUpDisabled <- true
		}()

		go func() {
			errCount := 0
			for range ticker.C {
				err := c.CleanUpPolling(ctx)
				if errCount <= 10 {
					log.Info("Polling failed with", "error, ", err.Error(),
						"RetryIn: ", strconv.Itoa(c.IBCleanUpInterval))
					errCount++
				} else {
					log.Info("Polling failed, max error count reached, ", "MaxErrorCount: ", errCount)
					cleanUpDisabled <- true
				}
			}
		}()
	} else {
		cleanUpDisabled <- true
	}

	disabled := <-cleanUpDisabled
	if disabled {
		log.Info("Auto IB clean up disabled, you must manually delete ImageBuild resources on your own")
		ticker.Stop()
	}

	return ctrl.Result{}, nil
}

func (c *IBCleanUpComponent) CleanUpPolling(ctx *core.Context) error {
	log := ctx.Log
	ibList := &hephv1.ImageBuildList{}
	err := ctx.Client.List(ctx, ibList)

	if err != nil {
		log.Info("Unable to access a list of IBs, not starting IB clean up")
		return err
	}

	listLen := len(ibList.Items)
	if listLen == 0 {
		log.V(1).Info("No build resources found, aborting")
		return nil
	}
	log.Info("Fetched all build resources", "count", listLen)
	log.V(1).Info("Filtering image builds by state", "states", []hephv1.Phase{
		hephv1.PhaseSucceeded, hephv1.PhaseFailed,
	})

	var builds []hephv1.ImageBuild
	for _, ib := range ibList.Items {
		state := ib.Status.Phase
		if state == hephv1.PhaseSucceeded || state == hephv1.PhaseFailed {
			builds = append(builds, ib)
		}
	}

	if len(builds) <= c.RetentionCount {
		log.Info("Total resources are less than or equal to retention limit, aborting",
			"resourceCount", len(builds), "RetentionCount", c.RetentionCount)
		return nil
	}

	log.Info("Total resources eligible for deletion", "count", len(builds))

	for i, build := range builds[:len(builds)-c.RetentionCount] {
		if err := ctx.Client.Delete(ctx, &builds[i]); err != nil {
			log.Error(err, "Failed to delete build", "name", build.Name, "namespace", build.Namespace)
		}
		log.Info("Deleted build", "name", build.Name, "namespace", build.Namespace)
	}
	log.Info("Cleanup complete")
	return nil
}
