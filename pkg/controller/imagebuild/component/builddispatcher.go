package component

import (
	"fmt"
	"os"
	"time"

	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/discovery"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/phase"
)

type BuildDispatcherComponent struct {
	cfg   config.Buildkit
	phase *phase.TransitionHelper
}

func BuildDispatcher(cfg config.Buildkit) *BuildDispatcherComponent {
	return &BuildDispatcherComponent{
		cfg: cfg,
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

	return nil
}

func (c *BuildDispatcherComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageBuild)

	/*
		determine if we need to reconcile object
	*/
	if obj.Status.Phase != "" {
		log.Info("Resource phase in not blank, ignoring", "phase", obj.Status.Phase)
		return ctrl.Result{}, nil
	}

	/*
		gather information and prepare for build
	*/
	start := time.Now()
	c.phase.SetInitializing(ctx, obj)

	log.Info("Querying for buildkit service")
	addr, err := discovery.BuildkitService(ctx, c.cfg)
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("buildkit service lookup failed: %w", err))
	}

	log.Info("Processing registry credentials")
	configDir, err := credentials.Persist(ctx, ctx.Config, obj.Spec.RegistryAuth)
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("registry credentials processing failed: %w", err))
	}
	defer os.RemoveAll(configDir)

	log.Info("Building new buildkit client")
	bk, err := buildkit.
		RemoteClient(ctx, addr).
		WithLogger(ctx.Log.WithName("buildkit").WithValues("addr", addr)).
		WithMTLSAuth(c.cfg.CACertPath, c.cfg.CertPath, c.cfg.KeyPath).
		WithDockerAuthConfig(configDir).
		Build()
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, err)
	}

	/*
		dispatch remote build
	*/
	c.phase.SetRunning(ctx, obj)

	buildOpts := buildkit.BuildOptions{
		Context:            obj.Spec.Context,
		Images:             obj.Spec.Images,
		BuildArgs:          obj.Spec.BuildArgs,
		CacheTag:           fmt.Sprintf("%s-buildcache", obj.Spec.CacheTag), // NOTE: maybe this belongs elsewhere?
		CacheMode:          obj.Spec.CacheMode,
		DisableCacheExport: obj.Spec.DisableCacheExports,
		DisableCacheImport: obj.Spec.DisableCacheImports,
	}
	if err = bk.Build(buildOpts); err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("build failed: %w", err))
	}

	/*
		record final metadata and report success
	*/
	obj.Status.BuildTime = time.Since(start).String()
	c.phase.SetSucceeded(ctx, obj)

	log.Info("Reconciliation complete")
	return ctrl.Result{}, nil
}
