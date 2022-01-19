package component

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/dominodatalab/controller-util/core"
	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/discovery"
	"github.com/dominodatalab/hephaestus/pkg/controller/support/phase"
)

type CacheWarmerComponent struct {
	cfg        config.Buildkit
	log        logr.Logger
	client     client.Client
	config     config.Buildkit
	timeWindow time.Duration
	phase      *phase.TransitionHelper
}

func CacheWarmer(cfg config.Buildkit) *CacheWarmerComponent {
	return &CacheWarmerComponent{
		cfg: cfg,
	}
}

func (c *CacheWarmerComponent) GetReadyCondition() string {
	return "BuilderCacheReady"
}

func (c *CacheWarmerComponent) Initialize(ctx *core.Context, bldr *ctrl.Builder) error {
	c.log = ctx.Log
	c.client = ctx.Client
	c.timeWindow = 10 * time.Minute
	c.phase = &phase.TransitionHelper{
		Client: ctx.Client,
		ConditionMeta: phase.TransitionConditions{
			Initialize: func() (string, string) { return "Setup", "Starting warming process" },
			Running:    func() (string, string) { return "Dispatch", "Builders are caching image layers" },
			Success:    func() (string, string) { return "CacheSynced", "Image layers exported on all builders" },
		},
		ReadyCondition: c.GetReadyCondition(),
	}

	bldr.Watches(
		&source.Kind{Type: &corev1.Pod{}},
		handler.EnqueueRequestsFromMapFunc(c.mapBuildkitPodChanges),
		builder.WithPredicates(predicate.Funcs{CreateFunc: func(event.CreateEvent) bool { return true }}),
		// &eventhandler.PodMonitor{
		// 	Log:        ctx.Log,
		// 	Client:     ctx.Client,
		// 	Config:     ctx.Data["config"].(config.Buildkit),
		// 	TimeWindow: 10 * time.Minute,
		// },
	)

	return nil
}

func (c *CacheWarmerComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	// TODO: signal cache build stop on resource delete

	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageCache)

	/*
		1. Determine if a cache run is required
	*/
	synced := true

	var podList corev1.PodList
	listOpts := []client.ListOption{
		client.InNamespace(c.cfg.Namespace),
		client.MatchingLabels(c.cfg.Labels),
	}

	log.Info("Querying for buildkitd pods", "labels", c.cfg.Labels, "namespace", c.cfg.Namespace)
	if err := ctx.Client.List(ctx, &podList, listOpts...); err != nil {
		return ctrl.Result{}, fmt.Errorf("buildkitd pod lookup failed: %w", err)
	}
	if len(podList.Items) == 0 {
		return ctrl.Result{}, errors.New("no buildkitd pods found")
	}

	var podNames []string
	for _, pod := range podList.Items {
		podNames = append(podNames, pod.Name)
	}
	log.Info("Found buildkitd pods", "pods", podNames)

	if !reflect.DeepEqual(podNames, obj.Status.BuildkitPods) {
		synced = false
	}
	if !reflect.DeepEqual(obj.Spec.Images, obj.Status.CachedImages) {
		synced = false
	}

	if synced {
		log.Info("Resource synced, quitting reconciliation")
		return ctrl.Result{}, nil
	}

	/*
		2. Load external data required for building
	*/
	c.phase.SetInitializing(ctx, obj)

	log.Info("Querying for buildkitd endpoints")
	endpoints, err := discovery.BuildkitEndpoints(ctx, c.cfg)
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("buildkit worker lookup failed: %w", err))
	}

	if len(endpoints) < len(podNames) {
		log.Info("Not all buildkitd pods ready, requeuing event", "pods", podNames, "endpoints", endpoints)
		ctx.Conditions.SetFalse(
			c.GetReadyCondition(),
			"EndpointsMissing",
			fmt.Sprintf("Buildkitd workers %v missing endpoints %v", podNames, endpoints),
		)

		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	log.Info("Processing registry credentials")
	configDir, err := credentials.Persist(ctx, ctx.Config, obj.Spec.RegistryAuth)
	if err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("registry credentials processing failed: %w", err))
	}
	defer os.RemoveAll(configDir)

	/*
		3. Dispatch concurrent cache operations
	*/
	c.phase.SetRunning(ctx, obj)

	log.Info("Launching cache operation", "endpoints", endpoints, "images", obj.Spec.Images)
	eg, _ := errgroup.WithContext(ctx)
	for idx, addr := range endpoints {
		for _, image := range obj.Spec.Images {
			// close over variables
			idx := idx
			addr := addr
			image := image

			eg.Go(func() error {
				log := log.WithValues("addr", addr, "image", image)

				bkOpts := []buildkit.ClientOpt{
					buildkit.WithAuthConfig(configDir),
					buildkit.WithLogger(ctx.Log.WithName("buildkit").WithValues("addr", addr, "image", image)),
				}

				log.Info("Creating new buildkit client")
				var bk buildkit.Client
				if bk, err = buildkit.NewRemoteClient(ctx, addr, bkOpts...); err != nil {
					return err
				}

				builderCondition := fmt.Sprintf("BuildkitdPod-%d", idx)

				log.Info("Launching cache export")
				ctx.Conditions.SetUnknown(builderCondition, "LaunchingCacheRun", "")

				if err = bk.Cache(image); err != nil {
					ctx.Conditions.SetFalse(builderCondition, "CacheRunFailed", err.Error())
					return err
				}

				log.Info("Cache export complete")
				ctx.Conditions.SetTrue(builderCondition, "CacheRunSucceeded", "")

				return nil
			})
		}
	}

	if err = eg.Wait(); err != nil {
		return ctrl.Result{}, c.phase.SetFailed(ctx, obj, fmt.Errorf("caching operation failed: %w", err))
	}

	/*
		4. Record build metadata and report success
	*/
	obj.Status.BuildkitPods = podNames
	obj.Status.CachedImages = obj.Spec.Images
	c.phase.SetSucceeded(ctx, obj)

	log.Info("Reconciliation complete")
	return ctrl.Result{}, nil
}

func (c *CacheWarmerComponent) mapBuildkitPodChanges(obj client.Object) (requests []reconcile.Request) {
	if len(c.config.Labels) > len(obj.GetLabels()) {
		return
	}
	for k, v := range c.config.Labels {
		if ov, found := obj.GetLabels()[k]; !found || ov != v {
			return
		}
	}

	// NOTE: work through the permutations
	ageLimit := time.Now().Add(-c.timeWindow)
	if obj.GetCreationTimestamp().Time.Before(ageLimit) {
		return
	}

	cacheList := &hephv1.ImageCacheList{}
	err := c.client.List(context.Background(), cacheList)
	if err != nil {
		c.log.Error(err, "cannot list image cache objects")
	}

	requests = make([]reconcile.Request, len(cacheList.Items))
	for idx, ic := range cacheList.Items {
		requests[idx] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      ic.Name,
				Namespace: ic.Namespace,
			},
		}
	}
	return
}
