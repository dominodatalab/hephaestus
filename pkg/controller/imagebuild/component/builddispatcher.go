package component

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"github.com/newrelic/go-agent/v3/newrelic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/worker"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/phase"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/secrets"
)

var errNotRunning = errors.New("build not running")

type BuildDispatcherComponent struct {
	cfg      config.Buildkit
	pool     worker.Pool
	phase    *phase.TransitionHelper
	newRelic *newrelic.Application

	delete  <-chan client.ObjectKey
	cancels sync.Map
}

func BuildDispatcher(
	cfg config.Buildkit,
	pool worker.Pool,
	nr *newrelic.Application,
	ch <-chan client.ObjectKey,
) *BuildDispatcherComponent {
	return &BuildDispatcherComponent{
		cfg:      cfg,
		pool:     pool,
		delete:   ch,
		newRelic: nr,
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

//nolint:funlen
func (c *BuildDispatcherComponent) Reconcile(coreCtx *core.Context) (ctrl.Result, error) {
	obj := coreCtx.Object.(*hephv1.ImageBuild)

	log := coreCtx.Log

	buildLog := log.WithValues("logKey", obj.Spec.LogKey)

	switch obj.Status.Phase {
	case hephv1.PhaseInitializing, hephv1.PhaseRunning:
		var err error
		if _, running := c.cancels.Load(obj.ObjectKey()); !running {
			err = c.phase.SetFailed(coreCtx, obj, errNotRunning)
		}
		return ctrl.Result{}, err

	case hephv1.PhaseSucceeded, hephv1.PhaseFailed:
		return ctrl.Result{}, nil
	case "":
		// new ImageBuild
	default:
		log.Info("Aborting reconcile, unknown status phase", "phase", obj.Status.Phase)
		return ctrl.Result{}, nil
	}

	buildCtx, cancel := context.WithCancel(coreCtx)
	c.cancels.Store(obj.ObjectKey(), cancel)
	defer func() {
		cancel()
		c.cancels.Delete(obj.ObjectKey())
	}()

	txn := c.newRelic.StartTransaction("BuildDispatcherComponent.Reconcile")
	txn.AddAttribute("imagebuild", obj.ObjectKey().String())
	defer txn.End()

	c.phase.SetInitializing(coreCtx, obj)

	// Extracts cluster secrets into data to pass to buildkit
	log.Info("Processing references to build secrets")
	secretsReadSeq := txn.StartSegment("cluster-secrets-read")
	secretsData, err := secrets.ReadSecrets(coreCtx, obj, log, coreCtx.Config, coreCtx.Scheme)
	if err != nil {
		err = fmt.Errorf("cluster secrets processing failed: %w", err)
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "ClusterSecretsReadError",
		})

		return ctrl.Result{}, c.phase.SetFailed(coreCtx, obj, err)
	}
	secretsReadSeq.End()

	log.Info("Processing and persisting registry credentials")
	persistCredsSeg := txn.StartSegment("credentials-persist")
	configDir, helpMessage, err := credentials.Persist(coreCtx, buildLog, coreCtx.Config, obj.Spec.RegistryAuth)
	if err != nil {
		err = fmt.Errorf("registry credentials processing failed: %w", err)
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "CredentialsPersistError",
		})

		return ctrl.Result{}, c.phase.SetFailed(coreCtx, obj, err)
	}
	persistCredsSeg.End()

	defer func(path string) {
		if err := os.RemoveAll(path); err != nil {
			log.Error(err, "Failed to delete registry credentials")
		}
	}(configDir)

	validateCredsSeg := txn.StartSegment("credentials-validate")

	insecureRegistries := make([]string, 0)
	for reg, opts := range c.cfg.Registries {
		if opts.Insecure || opts.HTTP {
			insecureRegistries = append(insecureRegistries, reg)
		}
	}

	buildLog.Info("Validating registry credentials")
	if err = credentials.Verify(coreCtx, configDir, insecureRegistries, helpMessage); err != nil {
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "CredentialsValidateError",
		})

		buildLog.Error(err, fmt.Sprintf("Failed to validate registry credentials: %s", err.Error()))
		return ctrl.Result{}, c.phase.SetFailed(coreCtx, obj, err)
	}
	validateCredsSeg.End()

	log.Info("Leasing buildkit worker")
	buildLog.Info("Leasing buildkit worker")

	leaseSeg := txn.StartSegment("worker-lease")
	allocStart := time.Now()
	addr, err := c.pool.Get(coreCtx, obj.ObjectKey().String())
	if err != nil {
		buildLog.Error(err, fmt.Sprintf("Failed to acquire buildkit worker: %s", err.Error()))
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "WorkerLeaseError",
		})

		return ctrl.Result{}, c.phase.SetFailed(coreCtx, obj, fmt.Errorf("buildkit service lookup failed: %w", err))
	}
	leaseSeg.End()

	obj.Status.BuilderAddr = addr
	obj.Status.AllocationTime = time.Since(allocStart).Truncate(time.Millisecond).String()

	defer func(pool worker.Pool, endpoint string) {
		log.Info("Releasing buildkit worker", "endpoint", endpoint)
		if err := pool.Release(buildCtx, endpoint); err != nil {
			log.Error(err, "Failed to release pool endpoint", "endpoint", endpoint)
		} else {
			log.Info("Buildkit worker released")
		}
	}(c.pool, addr)

	log.Info("Building new buildkit client", "addr", addr)
	clientInitSeg := txn.StartSegment("worker-client-init")
	bldr := buildkit.
		NewClientBuilder(addr).
		WithLogger(coreCtx.Log.WithName("buildkit").WithValues("addr", addr, "logKey", obj.Spec.LogKey)).
		WithDockerConfigDir(configDir)
	if mtls := c.cfg.MTLS; mtls != nil {
		bldr.WithMTLSAuth(mtls.CACertPath, mtls.CertPath, mtls.KeyPath)
	}

	bk, err := bldr.Build(buildCtx)
	if err != nil {
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "WorkerClientInitError",
		})
		return ctrl.Result{}, c.phase.SetFailed(coreCtx, obj, err)
	}
	clientInitSeg.End()

	buildOpts := buildkit.BuildOptions{
		Context:                  obj.Spec.Context,
		Images:                   obj.Spec.Images,
		BuildArgs:                obj.Spec.BuildArgs,
		NoCache:                  obj.Spec.DisableLocalBuildCache,
		ImportCache:              obj.Spec.ImportRemoteBuildCache,
		DisableInlineCacheExport: obj.Spec.DisableCacheLayerExport,
		Secrets:                  c.cfg.Secrets,
		SecretsData:              secretsData,
		FetchAndExtractTimeout:   c.cfg.FetchAndExtractTimeout,
	}
	log.Info("Dispatching image build", "images", buildOpts.Images)

	c.phase.SetRunning(coreCtx, obj)
	buildSeg := txn.StartSegment("image-build")
	start := time.Now()

	// best effort phase change regardless if the original context is "done"
	coreCtx.Context = context.Background()
	imageSize, err := bk.Build(buildCtx, buildOpts)
	if err != nil {
		// if the underlying buildkit pod is terminated via resource delete, then buildCtx will be closed and there will
		// be an error on it. otherwise, some external event (e.g. pod terminated) cancelled the build, so we should
		// mark the build as failed.
		if buildCtx.Err() != nil {
			log.Info("Build cancelled via resource delete")
			txn.AddAttribute("cancelled", true)

			//nolint:nilerr // we want reconciliation to pass and end her
			return ctrl.Result{}, nil
		}

		buildLog.Error(err, fmt.Sprintf("Failed to build image: %s", err.Error()))

		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "ImageBuildError",
		})
		return ctrl.Result{}, c.phase.SetFailed(coreCtx, obj, fmt.Errorf("build failed: %w", err))
	}
	obj.Status.BuildTime = "foobar"
	buildSeg.End()

	log.Info("Final image size: ", "imageSize", imageSize)
	annotations := obj.GetAnnotations()
	annotations["imagebuilder.dominodatalab.com/image-size"] = strconv.FormatInt(imageSize, 10)
	obj.Status.CompressedImageSize = strconv.FormatInt(imageSize, 10)
	obj.SetAnnotations(annotations)

	annotations = obj.GetAnnotations()
	log.Info("Final annotations: ",
		"annotations", annotations,
		"uid", obj.GetUID(),
		"name", obj.GetName(),
		"status", obj.Status,
		"buildtime", time.Since(start).Truncate(time.Millisecond).String())
	c.phase.SetSucceeded(coreCtx, obj)
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
