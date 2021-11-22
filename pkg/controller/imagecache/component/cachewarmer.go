package component

import (
	"fmt"
	"os"

	"github.com/dominodatalab/controller-util/core"
	"golang.org/x/sync/errgroup"
	ctrl "sigs.k8s.io/controller-runtime"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/discovery"
)

type CacheWarmerComponent struct{}

func CacheWarmer() *CacheWarmerComponent {
	return &CacheWarmerComponent{}
}

func (c *CacheWarmerComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageCache)
	cfg := ctx.Data["config"].(config.BuildkitConfig)

	log.Info("Reconciliation start", "images", obj.Spec.Images)

	log.V(1).Info("Looking up buildkitd workers")
	endpoints, err := discovery.LookupEndpoints(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("buildkit worker lookup failed: %w", err)
	}

	log.V(1).Info("Processing registry credentials")
	configDir, err := discovery.ProcessCredentials(obj.Spec.RegistryAuth)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer os.RemoveAll(configDir)

	eg, _ := errgroup.WithContext(ctx)
	for _, ep := range endpoints {
		// capture variable in closure to avoid race condition
		addr := ep

		eg.Go(func() (err error) {
			log.V(1).Info("Creating new buildkit client")
			client, err := buildkit.NewRemoteClient(ctx, addr,
				buildkit.WithLogger(ctx.Log),
				buildkit.WithAuthConfig(configDir),
			)
			if err != nil {
				return err
			}

			for _, image := range obj.Spec.Images {
				log := log.WithValues("addr", addr, "image", image)

				log.Info("Launching caching export")
				if err = client.Cache(image); err != nil {
					return fmt.Errorf("buildkit error: %w", err)
				}

				log.Info("Caching export complete")
			}

			return nil
		})
	}

	if err = eg.Wait(); err != nil {
		return ctrl.Result{}, fmt.Errorf("caching operation failed: %w", err)
	}

	log.Info("Reconciliation complete")
	return ctrl.Result{}, nil
}
