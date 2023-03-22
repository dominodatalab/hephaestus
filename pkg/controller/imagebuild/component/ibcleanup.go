package component

import (
	"github.com/dominodatalab/hephaestus/pkg/config"
	"strconv"
	"time"

	"github.com/dominodatalab/controller-util/core"
	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

type IBCleanUpComponent struct {
	RetentionCount    int
	IBCleanUpInterval int
	Enabled           bool
}

func IBCleanUp(cfg config.CleanUp) *IBCleanUpComponent {
	return &IBCleanUpComponent{
		cfg.MaxIBRetention,
		cfg.IBCleanUpInterval,
		cfg.Enabled,
	}
}

func (c *IBCleanUpComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	interval := time.Duration(c.IBCleanUpInterval) * time.Minute
	ticker := time.NewTicker(interval)
	done := make(chan bool)
	go c.startPolling(ticker, ctx, done)

	<-done
	ticker.Stop()
	log.Info("Auto Cleanup is disabled, please manually clean up your Image Build resources")
	return ctrl.Result{}, nil
}

func (c *IBCleanUpComponent) startPolling(ticker *time.Ticker, ctx *core.Context, done chan bool) {
	if !c.Enabled {
		done <- true
		return
	}

	// Keep track of how many times the function has failed in a row
	failureCount := 0
	maxRetries := 10
	for range ticker.C {
		err := c.CleanUpPolling(ctx)
		if err == nil {
			failureCount = 0
		} else {
			failureCount++
			if failureCount >= maxRetries {
				// Disable clean up component on too many retries
				done <- true
				c.Enabled = false
				return
			}

			// Log error and retry information
			ctx.Log.Info("Polling failed with", "error, ", err.Error(),
				"RetryIn: ", strconv.Itoa(c.IBCleanUpInterval), "RetriesLeft", strconv.Itoa(maxRetries-failureCount))
		}
	}
}

func (c *IBCleanUpComponent) CleanUpPolling(ctx *core.Context) error {
	log := ctx.Log
	ibList := &hephv1.ImageBuildList{}
	if err := ctx.Client.List(ctx, ibList); err != nil {
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

	builds := make([]hephv1.ImageBuild, 0)
	for _, ib := range ibList.Items {
		if ib.Status.Phase == hephv1.PhaseSucceeded || ib.Status.Phase == hephv1.PhaseFailed {
			builds = append(builds, ib)
		}
	}

	if len(builds) <= c.RetentionCount {
		log.V(1).Info("Total resources are less than or equal to retention limit, aborting",
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
	log.Info("IB cleanup complete")
	return nil
}
