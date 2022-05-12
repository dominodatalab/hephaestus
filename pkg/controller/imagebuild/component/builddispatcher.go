package component

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/workerpool"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/phase"
)

type BuildDispatcherComponent struct {
	cfg   config.Buildkit
	pool  workerpool.Pool
	phase *phase.TransitionHelper

	delete  <-chan client.ObjectKey
	cancels sync.Map
}

func BuildDispatcher(cfg config.Buildkit, pool workerpool.Pool, ch <-chan client.ObjectKey) *BuildDispatcherComponent {
	return &BuildDispatcherComponent{
		cfg:    cfg,
		pool:   pool,
		delete: ch,
	}
}

func (c *BuildDispatcherComponent) GetReadyCondition() string {
	return "ImageReady"
}

func (c *BuildDispatcherComponent) Initialize(ctx *core.Context, _ *ctrl.Builder) error {
	c.phase = &phase.TransitionHelper{
		Client: ctx.Client,
		ConditionMeta: phase.TransitionConditions{
			Initialize: func() (string, string) { return "Setup", "Processing build parameters" },
			Running:    func() (string, string) { return "BuildingImage", "Running image build in buildkit" },
			Success:    func() (string, string) { return "BuildComplete", "Image has been built and pushed to registry" },
		},
		ReadyCondition: c.GetReadyCondition(),
	}

	go c.processCancellations(ctx.Log)

	return nil
}

func (c *BuildDispatcherComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageBuild)

	buildCtx, cancel := context.WithCancel(ctx)
	c.cancels.Store(obj.ObjectKey(), cancel)
	defer func() {
		cancel()
		c.cancels.Delete(obj.ObjectKey())
	}()

	if obj.Status.Phase != hephv1.PhaseCreated {
		log.Info("Aborting reconcile, status phase in not blank", "phase", obj.Status.Phase)
		return ctrl.Result{}, nil
	}

	start := time.Now()
	c.phase.SetInitializing(ctx, obj)

	log.Info("Processing and persisting registry credentials")
	configDir, err := credentials.Persist(ctx, ctx.Config, obj.Spec.RegistryAuth)
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("registry credentials processing failed: %w", err))
	}

	defer func(path string) {
		if err := os.RemoveAll(path); err != nil {
			log.Error(err, "Failed to delete registry credentials")
		}
	}(configDir)

	log.Info("Leasing buildkit worker")
	future, err := c.pool.Get(ctx)
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("buildkit service lookup failed: %w", err))
	}

	addr, err := future()
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("buildkit service lookup failed: %w", err))
	}

	defer func(pool workerpool.Pool, endpoint string) {
		log.Info("Releasing buildkit worker", "endpoint", endpoint)
		if err := pool.Release(endpoint); err != nil {
			log.Error(err, "Failed to release pool endpoint", "endpoint", endpoint)
		}

		log.Info("Buildkit worker released")
	}(c.pool, addr)

	log.Info("Building new buildkit client", "addr", addr)
	bldr := buildkit.
		ClientBuilder(addr).
		WithLogger(ctx.Log.WithName("buildkit").WithValues("addr", addr, "logKey", obj.Spec.LogKey)).
		WithDockerAuthConfig(configDir)
	if mtls := c.cfg.MTLS; mtls != nil {
		bldr.WithMTLSAuth(mtls.CACertPath, mtls.CertPath, mtls.KeyPath)
	}

	bk, err := bldr.Build(context.Background())
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, err)
	}

	c.phase.SetRunning(ctx, obj)

	buildOpts := buildkit.BuildOptions{
		Context:                  obj.Spec.Context,
		Images:                   obj.Spec.Images,
		BuildArgs:                obj.Spec.BuildArgs,
		NoCache:                  obj.Spec.DisableLocalBuildCache,
		ImportCache:              obj.Spec.ImportRemoteBuildCache,
		DisableInlineCacheExport: obj.Spec.DisableCacheLayerExport,
	}

	log.Info("Dispatching image build", "images", buildOpts.Images)
	if err = bk.Build(buildCtx, buildOpts); err != nil {
		// buildkit uses pkg/errors for wrapping and the underlying error is a grpcerror with a status that contains the
		// context cancellation message, not the error value. the easiest way to assert that the context was cancelled.
		if strings.HasSuffix(err.Error(), context.Canceled.Error()) {
			log.Info("Build cancelled")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("build failed: %w", err))
	}

	obj.Status.BuildTime = time.Since(start).String()
	c.phase.SetSucceeded(ctx, obj)

	return ctrl.Result{}, nil
}

func (c *BuildDispatcherComponent) processCancellations(log logr.Logger) {
	for objKey := range c.delete {
		log := log.WithValues("imagebuild", objKey)

		log.Info("Intercepted delete message")
		if v, ok := c.cancels.LoadAndDelete(objKey); ok {
			log.Info("Found cancellation")
			v.(context.CancelFunc)()
			log.Info("Context cancelled")

			continue
		}
		log.Info("Ignoring message, cancellation not found")
	}
}
