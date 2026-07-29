package component

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/newrelic/go-agent/v3/newrelic"
	"golang.org/x/sync/errgroup"
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

// releaseTimeout bounds how long a worker release is allowed to take once detached from a
// cancelled build context (see buildPlatform), so a release call can't hang forever.
const releaseTimeout = 30 * time.Second

type BuildDispatcherComponent struct {
	cfg      config.Buildkit
	pools    worker.PoolSet
	phase    *phase.TransitionHelper
	newRelic *newrelic.Application

	// primaryPoolName is used for builds that request no platforms at all, preserving exact
	// pre-multi-arch behavior: it's always the first entry in cfg.Pools(), which is the single
	// synthesized "default" pool whenever PlatformPools is unset.
	primaryPoolName string

	delete  <-chan client.ObjectKey
	cancels sync.Map
}

func BuildDispatcher(
	cfg config.Buildkit,
	pools worker.PoolSet,
	nr *newrelic.Application,
	ch <-chan client.ObjectKey,
) *BuildDispatcherComponent {
	return &BuildDispatcherComponent{
		cfg:             cfg,
		pools:           pools,
		primaryPoolName: cfg.Pools()[0].Name,
		delete:          ch,
		newRelic:        nr,
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

func (c *BuildDispatcherComponent) Reconcile(coreCtx *core.Context) (ctrl.Result, error) {
	obj := coreCtx.Object.(*hephv1.ImageBuild)

	log := coreCtx.Log

	buildLog := log.WithValues("logKey", obj.Spec.LogKey)

	switch obj.Status.Phase {
	case hephv1.PhaseInitializing, hephv1.PhaseRunning:
		var err error
		if _, running := c.cancels.Load(obj.ObjectKey()); !running {
			err = c.failBuild(coreCtx, obj, errNotRunning, "NotRunning")
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

	secretsData, configDir, insecureRegistries, err := c.prepareBuildInputs(coreCtx, obj, log, buildLog, txn)
	if configDir != "" {
		defer func(path string) {
			if err := os.RemoveAll(path); err != nil {
				log.Error(err, "Failed to delete registry credentials")
			}
		}(configDir)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	groups, err := c.resolvePlatformGroups(obj.Spec.Platforms)
	if err != nil {
		return ctrl.Result{}, c.failBuild(coreCtx, obj, err, "PlatformResolutionError")
	}

	if len(groups) > 1 {
		return ctrl.Result{}, c.reconcileFanOut(buildCtx, coreCtx, obj, txn, log, buildLog,
			secretsData, configDir, insecureRegistries, groups)
	}

	var poolName string
	var platforms []string
	for name, ps := range groups {
		poolName, platforms = name, ps
	}

	pool, ok := c.pools.Get(poolName)
	if !ok {
		return ctrl.Result{}, c.failBuild(coreCtx, obj,
			fmt.Errorf("pool %q is not configured", poolName), "PoolNotConfigured")
	}

	log.Info("Leasing buildkit worker")
	buildLog.Info("Leasing buildkit worker")

	leaseSeg := txn.StartSegment("worker-lease")
	allocStart := time.Now()
	addr, err := pool.Get(coreCtx, obj.ObjectKey().String())
	if err != nil {
		buildLog.Error(err, fmt.Sprintf("Failed to acquire buildkit worker: %s", err.Error()))
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "WorkerLeaseError",
		})

		return ctrl.Result{}, c.failBuild(coreCtx, obj,
			fmt.Errorf("buildkit service lookup failed: %w", err), "WorkerLeaseError")
	}
	leaseSeg.End()

	obj.Status.BuilderAddr = addr
	obj.Status.AllocationTime = time.Since(allocStart).Truncate(time.Millisecond).String()

	defer func(pool worker.Pool, endpoint string) {
		log.Info("Releasing buildkit worker", "endpoint", endpoint)
		if err := pool.Release(coreCtx, endpoint); err != nil {
			log.Error(err, "Failed to release pool endpoint", "endpoint", endpoint)
		} else {
			log.Info("Buildkit worker released")
		}
	}(pool, addr)

	log.Info("Building new buildkit client", "addr", addr)
	clientInitSeg := txn.StartSegment("worker-client-init")

	// Create refreshing auth provider for cloud registries (ACR/ECR/GCR)
	authProvider := buildkit.NewRefreshingAuthProvider(
		credentials.CloudAuthRegistry,
		configDir,
		buildLog,
	)

	bldr := buildkit.
		NewClientBuilder(addr).
		WithLogger(coreCtx.Log.WithName("buildkit").WithValues("addr", addr, "logKey", obj.Spec.LogKey)).
		WithDockerConfigDir(configDir).
		WithAuthProvider(authProvider)
	if mtls := c.cfg.MTLS; mtls != nil {
		bldr.WithMTLSAuth(mtls.CACertPath, mtls.CertPath, mtls.KeyPath)
	}

	bk, err := bldr.Build(buildCtx)
	if err != nil {
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "WorkerClientInitError",
		})
		return ctrl.Result{}, c.failBuild(coreCtx, obj, err, "WorkerClientInitError")
	}
	clientInitSeg.End()

	buildOpts := buildkit.BuildOptions{
		Context:                  obj.Spec.Context,
		DockerfileContents:       obj.Spec.DockerfileContents,
		Images:                   obj.Spec.Images,
		BuildArgs:                obj.Spec.BuildArgs,
		NoCache:                  obj.Spec.DisableLocalBuildCache,
		ImportCache:              obj.Spec.ImportRemoteBuildCache,
		DisableInlineCacheExport: obj.Spec.DisableCacheLayerExport,
		Secrets:                  c.cfg.Secrets,
		SecretsData:              secretsData,
		FetchAndExtractTimeout:   c.cfg.FetchAndExtractTimeout,
		Platforms:                platforms,
	}
	log.Info("Dispatching image build", "images", buildOpts.Images)

	c.phase.SetRunning(coreCtx, obj)
	buildSeg := txn.StartSegment("image-build")
	start := time.Now()

	// best effort phase change regardless if the original context is "done"
	coreCtx.Context = context.Background()
	result, err := bk.Build(buildCtx, buildOpts)
	if err != nil {
		// if the underlying buildkit pod is terminated via resource delete, then buildCtx will be closed and there will
		// be an error on it. otherwise, some external event (e.g. pod terminated) cancelled the build, so we should
		// mark the build as failed.
		if buildCtx.Err() != nil {
			log.Info("Build cancelled via resource delete")
			txn.AddAttribute("cancelled", true)

			return ctrl.Result{}, nil
		}

		buildLog.Error(err, fmt.Sprintf("Failed to build image: %s", err.Error()))

		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "ImageBuildError",
		})
		return ctrl.Result{}, c.failBuild(coreCtx, obj, fmt.Errorf("build failed: %w", err), "ImageBuildError")
	}
	obj.Status.BuildTime = time.Since(start).Truncate(time.Millisecond).String()
	buildSeg.End()

	populateImageStatus(buildCtx, bk, obj, result, log, buildLog, insecureRegistries)

	return ctrl.Result{}, c.succeedBuild(coreCtx, obj)
}

// prepareBuildInputs reads referenced cluster secrets and processes/validates registry credentials
// shared by every platform build in this reconcile. On any failure it calls failBuild itself and
// returns that error; callers should treat a non-nil error as already terminal. configDir is
// returned even on a validation failure (after Persist has succeeded) so the caller can still clean
// it up, and is "" when returned before Persist ever ran.
func (c *BuildDispatcherComponent) prepareBuildInputs(
	coreCtx *core.Context, obj *hephv1.ImageBuild, log, buildLog logr.Logger, txn *newrelic.Transaction,
) (secretsData map[string][]byte, configDir string, insecureRegistries []string, err error) {
	log.Info("Processing references to build secrets")
	secretsReadSeg := txn.StartSegment("cluster-secrets-read")
	secretsData, err = secrets.ReadSecrets(coreCtx, obj, log, coreCtx.Config, coreCtx.Scheme)
	if err != nil {
		err = fmt.Errorf("cluster secrets processing failed: %w", err)
		txn.NoticeError(newrelic.Error{Message: err.Error(), Class: "ClusterSecretsReadError"})

		return nil, "", nil, c.failBuild(coreCtx, obj, err, "ClusterSecretsReadError")
	}
	secretsReadSeg.End()

	log.Info("Processing and persisting registry credentials")
	persistCredsSeg := txn.StartSegment("credentials-persist")
	var helpMessage []string
	configDir, helpMessage, err = credentials.Persist(coreCtx, buildLog, coreCtx.Config, obj.Spec.RegistryAuth)
	if err != nil {
		err = fmt.Errorf("registry credentials processing failed: %w", err)
		txn.NoticeError(newrelic.Error{Message: err.Error(), Class: "CredentialsPersistError"})

		return nil, "", nil, c.failBuild(coreCtx, obj, err, "CredentialsPersistError")
	}
	persistCredsSeg.End()

	validateCredsSeg := txn.StartSegment("credentials-validate")

	insecureRegistries = make([]string, 0)
	for reg, opts := range c.cfg.Registries {
		if opts.Insecure || opts.HTTP {
			insecureRegistries = append(insecureRegistries, reg)
		}
	}

	buildLog.Info("Validating registry credentials")
	if err = credentials.Verify(coreCtx, configDir, insecureRegistries, helpMessage); err != nil {
		txn.NoticeError(newrelic.Error{Message: err.Error(), Class: "CredentialsValidateError"})

		buildLog.Error(err, fmt.Sprintf("Failed to validate registry credentials: %s", err.Error()))
		return nil, configDir, nil, c.failBuild(coreCtx, obj, err, "CredentialsValidateError")
	}
	validateCredsSeg.End()

	return secretsData, configDir, insecureRegistries, nil
}

// failBuild records a terminal Failed transition for the build and increments the phase
// metric with the given reason. The metric is incremented only after the transition is
// durably persisted, so a reconcile requeue triggered by a failed status write does not
// double-count (the retry re-persists and increments exactly once). reason mirrors the
// New Relic error.class set at the call site. On a successful persist the original build
// error is returned unchanged, preserving the existing reconcile requeue/error behavior.
func (c *BuildDispatcherComponent) failBuild(
	ctx *core.Context, obj *hephv1.ImageBuild, err error, reason string,
) error {
	if persistErr := c.phase.SetFailed(ctx, obj, err); persistErr != nil {
		// Terminal phase not persisted; do not count. The non-nil return requeues the
		// reconcile, which retries the write and increments once it lands.
		return persistErr
	}

	recordImageBuildPhase(hephv1.PhaseFailed, reason)
	recordImageBuildPlatformPhases(obj.Status.Platforms, hephv1.PhaseFailed, reason)

	return err
}

// succeedBuild records a terminal Succeeded transition for the build and increments the
// phase metric (with an empty failure_reason). As with failBuild, the metric is
// incremented only after the transition is durably persisted; a failed status write is
// returned so the reconcile requeues and retries rather than silently counting a
// non-durable transition.
func (c *BuildDispatcherComponent) succeedBuild(ctx *core.Context, obj *hephv1.ImageBuild) error {
	if persistErr := c.phase.SetSucceeded(ctx, obj); persistErr != nil {
		return persistErr
	}

	recordImageBuildPhase(hephv1.PhaseSucceeded, "")
	recordImageBuildPlatformPhases(obj.Status.Platforms, hephv1.PhaseSucceeded, "")

	return nil
}

// resolvePlatformGroups maps the requested platforms to the pool(s) that must serve them. An empty
// platforms list resolves to a single group against the primary pool, preserving exact
// pre-multi-arch behavior. Every platform is guaranteed resolvable here since the validating
// webhook already rejected any platform unsupported by every configured pool; a resolution error is
// a defensive backstop, not an expected path.
func (c *BuildDispatcherComponent) resolvePlatformGroups(platforms []string) (map[string][]string, error) {
	if len(platforms) == 0 {
		return map[string][]string{c.primaryPoolName: nil}, nil
	}

	groups, err := c.cfg.PlatformCapabilities().ResolvePools(platforms)
	if err != nil {
		return nil, fmt.Errorf("platform resolution failed: %w", err)
	}

	return groups, nil
}

// platformAssignment pairs a single requested platform with the pool that will build it.
type platformAssignment struct {
	pool     string
	platform string
}

// flattenPlatformAssignments expands a pool -> platforms grouping into one assignment per
// platform, sorted for deterministic logging/ordering.
func flattenPlatformAssignments(groups map[string][]string) []platformAssignment {
	assignments := make([]platformAssignment, 0, len(groups))
	for pool, platforms := range groups {
		for _, platform := range platforms {
			assignments = append(assignments, platformAssignment{pool: pool, platform: platform})
		}
	}

	slices.SortFunc(assignments, func(a, b platformAssignment) int {
		if c := strings.Compare(a.platform, b.platform); c != 0 {
			return c
		}
		return strings.Compare(a.pool, b.pool)
	})

	return assignments
}

// platformBuild is the outcome of building one platform within a fan-out ImageBuild: bk is the
// buildkit client used for that platform's solve (needed afterward to retrieve the pushed image's
// size/labels, and to assemble the final manifest list, since auth resolution is a local
// configDir/authProvider lookup and any client sharing that configDir works regardless of which
// daemon produced the image). images are the per-platform-suffixed refs that were actually pushed.
type platformBuild struct {
	pool     string
	platform string
	images   []string
	bk       *buildkit.Client
	result   buildkit.SolveResult
	err      error
}

// platformImageSlug renders a platform string as an image-tag-safe suffix, e.g. "linux/arm64/v8"
// -> "linux-arm64-v8".
func platformImageSlug(platform string) string {
	return strings.ReplaceAll(platform, "/", "-")
}

// suffixImageRef appends "-<slug>" to an image reference's tag, producing a distinct per-platform
// intermediate tag. Untagged references are parsed as name.Tag with an implicit "latest" tag by
// name.ParseReference, so the tag branch covers them too; a digest reference falls back to tagging
// the same repository with "<default-tag>-<slug>", since a digest can't be suffixed in place.
func suffixImageRef(image, slug string) string {
	ref, err := name.ParseReference(image)
	if err != nil {
		return image + "-" + slug
	}
	if tag, ok := ref.(name.Tag); ok {
		return tag.Context().Tag(tag.TagStr() + "-" + slug).Name()
	}

	return ref.Context().Tag(name.DefaultTag + "-" + slug).Name()
}

// suffixImageRefs applies suffixImageRef to every image, for the given platform.
func suffixImageRefs(images []string, platform string) []string {
	slug := platformImageSlug(platform)
	suffixed := make([]string, len(images))
	for i, image := range images {
		suffixed[i] = suffixImageRef(image, slug)
	}

	return suffixed
}

// reconcileFanOut builds one image per platform assignment concurrently, each leased from its own
// pool, then assembles the per-platform results into a single multi-platform manifest list/index
// pushed to each of obj.Spec.Images. The build fails atomically: if any platform's solve fails, the
// others are cancelled via the shared context and the whole ImageBuild is marked Failed, with
// per-platform errors recorded on obj.Status.Platforms for visibility.
func (c *BuildDispatcherComponent) reconcileFanOut(
	buildCtx context.Context,
	coreCtx *core.Context,
	obj *hephv1.ImageBuild,
	txn *newrelic.Transaction,
	log, buildLog logr.Logger,
	secretsData map[string][]byte,
	configDir string,
	insecureRegistries []string,
	groups map[string][]string,
) error {
	assignments := flattenPlatformAssignments(groups)
	log.Info("Dispatching multi-pool image build", "images", obj.Spec.Images, "assignments", assignments)

	c.phase.SetRunning(coreCtx, obj)
	fanOutSeg := txn.StartSegment("image-build-fanout")
	start := time.Now()

	// best effort phase change regardless if the original context is "done"
	coreCtx.Context = context.Background()
	builds := c.buildAllPlatforms(buildCtx, obj, assignments, secretsData, configDir, buildLog)

	obj.Status.BuildTime = time.Since(start).Truncate(time.Millisecond).String()
	fanOutSeg.End()

	obj.Status.Platforms = platformResultsFrom(buildCtx, builds, insecureRegistries, buildLog)

	if err := firstBuildError(builds); err != nil {
		// if the underlying buildkit pod is terminated via resource delete, then buildCtx will be closed and there
		// will be an error on it. otherwise, some external event (e.g. pod terminated) cancelled the build, so we
		// should mark the build as failed. mirrors the single-pool path in Reconcile.
		if buildCtx.Err() != nil {
			log.Info("Build cancelled via resource delete")
			txn.AddAttribute("cancelled", true)

			return nil //nolint:nilerr // cancellation is not a build failure, see comment above
		}

		buildLog.Error(err, "Failed to build one or more platforms")
		txn.NoticeError(newrelic.Error{Message: err.Error(), Class: "ImageBuildError"})

		return c.failBuild(coreCtx, obj, fmt.Errorf("build failed: %w", err), "ImageBuildError")
	}

	digest, err := assembleManifestLists(buildCtx, obj, builds, insecureRegistries)
	if err != nil {
		txn.NoticeError(newrelic.Error{Message: err.Error(), Class: "ManifestListAssemblyError"})

		return c.failBuild(coreCtx, obj,
			fmt.Errorf("manifest list assembly failed: %w", err), "ManifestListAssemblyError")
	}
	obj.Status.Digest = digest

	return c.succeedBuild(coreCtx, obj)
}

// buildAllPlatforms runs one buildPlatform call per assignment concurrently, cancelling the rest
// via the shared errgroup context as soon as any one fails.
func (c *BuildDispatcherComponent) buildAllPlatforms(
	buildCtx context.Context,
	obj *hephv1.ImageBuild,
	assignments []platformAssignment,
	secretsData map[string][]byte,
	configDir string,
	log logr.Logger,
) []platformBuild {
	eg, egCtx := errgroup.WithContext(buildCtx)
	builds := make([]platformBuild, len(assignments))

	for i, a := range assignments {
		eg.Go(func() error {
			images := suffixImageRefs(obj.Spec.Images, a.platform)
			builds[i] = c.buildPlatform(egCtx, obj, a.pool, a.platform, images, secretsData, configDir, log)
			return builds[i].err
		})
	}
	_ = eg.Wait()

	return builds
}

// buildPlatform leases a worker from the named pool and runs a single-platform solve against it,
// releasing the worker before returning. Errors are recorded on the returned platformBuild rather
// than returned directly, since callers need every platform's outcome even when one fails.
func (c *BuildDispatcherComponent) buildPlatform(
	ctx context.Context,
	obj *hephv1.ImageBuild,
	poolName, platform string,
	images []string,
	secretsData map[string][]byte,
	configDir string,
	log logr.Logger,
) platformBuild {
	pb := platformBuild{pool: poolName, platform: platform, images: images}

	pool, ok := c.pools.Get(poolName)
	if !ok {
		pb.err = fmt.Errorf("pool %q is not configured", poolName)
		return pb
	}

	log = log.WithValues("pool", poolName, "platform", platform)
	log.Info("Leasing buildkit worker")

	addr, err := pool.Get(ctx, obj.ObjectKey().String()+"/"+platform)
	if err != nil {
		pb.err = fmt.Errorf("buildkit service lookup failed: %w", err)
		return pb
	}
	defer func() {
		log.Info("Releasing buildkit worker", "endpoint", addr)

		// best effort release regardless of whether a sibling platform's failure already
		// cancelled ctx via the shared errgroup context
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := pool.Release(releaseCtx, addr); err != nil {
			log.Error(err, "Failed to release pool endpoint", "endpoint", addr)
		}
	}()

	log.Info("Building new buildkit client", "addr", addr)
	authProvider := buildkit.NewRefreshingAuthProvider(credentials.CloudAuthRegistry, configDir, log)
	bldr := buildkit.
		NewClientBuilder(addr).
		WithLogger(log.WithName("buildkit").WithValues("addr", addr, "logKey", obj.Spec.LogKey)).
		WithDockerConfigDir(configDir).
		WithAuthProvider(authProvider)
	if mtls := c.cfg.MTLS; mtls != nil {
		bldr.WithMTLSAuth(mtls.CACertPath, mtls.CertPath, mtls.KeyPath)
	}

	bk, err := bldr.Build(ctx)
	if err != nil {
		pb.err = err
		return pb
	}
	pb.bk = bk

	buildOpts := buildkit.BuildOptions{
		Context:                  obj.Spec.Context,
		DockerfileContents:       obj.Spec.DockerfileContents,
		Images:                   images,
		BuildArgs:                obj.Spec.BuildArgs,
		NoCache:                  obj.Spec.DisableLocalBuildCache,
		ImportCache:              obj.Spec.ImportRemoteBuildCache,
		DisableInlineCacheExport: obj.Spec.DisableCacheLayerExport,
		Secrets:                  c.cfg.Secrets,
		SecretsData:              secretsData,
		FetchAndExtractTimeout:   c.cfg.FetchAndExtractTimeout,
		Platforms:                []string{platform},
	}

	log.Info("Dispatching platform image build", "images", images)
	pb.result, pb.err = bk.Build(ctx, buildOpts)

	return pb
}

// firstBuildError returns the first error among builds, in assignment order, or nil if every
// platform succeeded.
func firstBuildError(builds []platformBuild) error {
	for _, b := range builds {
		if b.err != nil {
			return b.err
		}
	}

	return nil
}

// platformResultsFrom records each platform's outcome as a hephv1.PlatformResult: the digest
// BuildKit reported for a failed build's error, or - on success - that digest plus a best-effort
// size/labels enrichment fetched from the registry.
func platformResultsFrom(
	ctx context.Context, builds []platformBuild, insecureRegistries []string, log logr.Logger,
) []hephv1.PlatformResult {
	results := make([]hephv1.PlatformResult, len(builds))

	for i, b := range builds {
		pr := hephv1.PlatformResult{Platform: b.platform, Digest: b.result.Digest}
		if b.err != nil {
			pr.Error = b.err.Error()
			results[i] = pr
			continue
		}

		if b.bk != nil && len(b.images) > 0 {
			populatePlatformImageDetails(ctx, b.bk, &pr, b.images[0], insecureRegistries, log)
		}

		results[i] = pr
	}

	return results
}

// populatePlatformImageDetails best-effort enriches pr with the pushed image's compressed size and
// labels, fetched from the registry. Failures are logged, not returned, since the platform build
// itself already succeeded by the time this runs.
func populatePlatformImageDetails(
	ctx context.Context, bk *buildkit.Client, pr *hephv1.PlatformResult, imageName string,
	insecureRegistries []string, log logr.Logger,
) {
	img, err := retrieveImage(ctx, bk, imageName, insecureRegistries)
	if err != nil {
		log.Error(err, "Cannot retrieve platform image from registry", "platform", pr.Platform, "imageName", imageName)
		return
	}

	size, err := calculateImageSize(img)
	if err != nil {
		log.Error(err, "Cannot calculate platform image size", "platform", pr.Platform)
	} else {
		pr.CompressedImageSizeBytes = strconv.FormatInt(size, 10)
	}

	cfgFile, err := img.ConfigFile()
	if err != nil {
		log.Error(err, "Cannot calculate platform image labels", "platform", pr.Platform)
		return
	}

	pr.Labels = make(map[string]string)
	for key, value := range cfgFile.Config.Labels {
		if len(value) > 0 {
			pr.Labels[key] = value
		}
	}
}

// assembleManifestLists combines the per-platform pushed images into one manifest list/index per
// requested image ref, using any successful platform's buildkit client to do so (auth resolution
// doesn't depend on which daemon produced the image - see platformBuild). It returns the last
// assembled index's digest; every image ref's assembled index has identical content (and therefore
// digest) since they all reference the same set of per-platform images, just under different tags.
func assembleManifestLists(
	ctx context.Context, obj *hephv1.ImageBuild, builds []platformBuild, insecureRegistries []string,
) (string, error) {
	var bk *buildkit.Client
	for _, b := range builds {
		if b.bk != nil {
			bk = b.bk
			break
		}
	}
	if bk == nil {
		return "", errors.New("no successful platform build to assemble a manifest list from")
	}

	var digest string
	for imageIdx, imageRef := range obj.Spec.Images {
		var refs []buildkit.PlatformImageRef
		for _, b := range builds {
			if imageIdx >= len(b.images) {
				continue
			}
			refs = append(refs, buildkit.PlatformImageRef{Platform: b.platform, ImageRef: b.images[imageIdx]})
		}

		d, err := bk.AssembleManifestList(ctx, insecureRegistries, imageRef, refs)
		if err != nil {
			return "", fmt.Errorf("cannot assemble manifest list for %q: %w", imageRef, err)
		}
		digest = d
	}

	return digest, nil
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

// populateImageStatus records the pushed image's digest/size/labels onto obj.Status. A true
// multi-platform solve (more than one requested platform) produces a manifest list/index directly
// in result.Digest; it doesn't resolve to a single v1.Image the way retrieveImage expects, so that
// case just records the digest BuildKit reported. Otherwise (0 or 1 platforms - the pre-multi-arch
// behavior), the existing registry round-trip populates the full digest/size/labels breakdown.
func populateImageStatus(
	ctx context.Context,
	bk *buildkit.Client,
	obj *hephv1.ImageBuild,
	result buildkit.SolveResult,
	log, buildLog logr.Logger,
	insecureRegistries []string,
) {
	if len(obj.Spec.Platforms) > 1 {
		obj.Status.Digest = result.Digest
		return
	}

	img, err := retrieveImage(ctx, bk, result.ImageName, insecureRegistries)
	if err != nil {
		log.Error(err, "Cannot retrieve image from registry", "imageName", result.ImageName)
		buildLog.Error(err, "Cannot retrieve image from registry", "imageName", result.ImageName)
		return
	}
	populateBuildStatus(obj, buildLog, img, result.ImageName)
}

func retrieveImage(
	ctx context.Context,
	c *buildkit.Client,
	imageName string,
	insecureRegistries []string,
) (v1.Image, error) {
	ref, err := buildkit.ParseImageReference(imageName, insecureRegistries)
	if err != nil {
		return nil, err
	}

	auth, err := c.ResolveAuth(ctx, ref.Context().RegistryStr())
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(auth))
	if err != nil {
		return nil, err
	}
	return img, nil
}

func populateBuildStatus(obj *hephv1.ImageBuild, log logr.Logger, img v1.Image, imageName string) {
	imageSize, err := calculateImageSize(img)
	if err != nil {
		log.Error(err, "Cannot calculate image size", "imageName", imageName)
	}

	log.Info(fmt.Sprintf("Final image size: %d", imageSize))
	obj.Status.CompressedImageSizeBytes = strconv.FormatInt(imageSize, 10)

	obj.Status.Labels = make(map[string]string)
	imageConfigFile, err := img.ConfigFile()
	if err != nil {
		log.Error(err, "Cannot calculate image labels", "imageName", imageName)
	} else {
		for key, value := range imageConfigFile.Config.Labels {
			if len(value) > 0 {
				obj.Status.Labels[key] = value
			}
		}
	}

	digest, err := img.Digest()
	if err != nil {
		log.Error(err, "Cannot retrieve image digest", "imageName", imageName)
	} else {
		obj.Status.Digest = digest.String()
	}
}

func calculateImageSize(img v1.Image) (int64, error) {
	layers, err := img.Layers()
	if err != nil {
		return 0, err
	}

	var size int64
	for _, layer := range layers {
		compressedSize, err := layer.Size()
		if err != nil {
			return 0, err
		}
		size += compressedSize
	}
	return size, nil
}
