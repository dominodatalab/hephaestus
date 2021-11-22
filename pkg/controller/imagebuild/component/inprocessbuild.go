package component

import (
	"fmt"
	"math/rand"
	"os"

	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit"
	"github.com/dominodatalab/hephaestus/pkg/controller/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/discovery"
)

func InProcessBuild() *inProcessBuildComponent {
	return &inProcessBuildComponent{}
}

type inProcessBuildComponent struct{}

func (c *inProcessBuildComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageBuild)
	cfg := ctx.Data["config"].(config.BuildkitConfig)

	log.Info("Reconciliation start")

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

	addr := endpoints[rand.Intn(len(endpoints))]
	log.Info("Launching build", "addr", addr)
	bk, err := buildkit.NewRemoteClient(ctx, addr,
		buildkit.WithLogger(ctx.Log),
		buildkit.WithAuthConfig(configDir),
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	opts := buildkit.BuildOptions{
		Context:            obj.Spec.Context,
		Images:             obj.Spec.Images,
		BuildArgs:          obj.Spec.BuildArgs,
		DisableCacheExport: obj.Spec.DisableCacheExports,
		DisableCacheImport: obj.Spec.DisableCacheImports,
	}
	if err = bk.Build(opts); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Reconciliation complete")
	return ctrl.Result{}, nil
}
