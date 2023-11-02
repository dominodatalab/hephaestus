package imagebuild

import (
	"github.com/dominodatalab/controller-util/core"
	"github.com/newrelic/go-agent/v3/newrelic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/buildkit/worker"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/component"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuild/predicate"
)

func Register(mgr ctrl.Manager,
	cfg config.Controller,
	pool worker.Pool,
	nr *newrelic.Application,
	deleteChan chan client.ObjectKey,
) error {
	err := core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}).
		Component("build-dispatcher", component.BuildDispatcher(cfg.Buildkit, pool, nr, deleteChan)).
		WithControllerOptions(controller.Options{MaxConcurrentReconciles: cfg.Manager.ImageBuild.Concurrency}).
		WithWebhooks().
		Complete()
	if err != nil {
		return err
	}

	namespaces := cfg.Manager.WatchNamespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	return mgr.Add(&component.ImageBuildGC{
		HistoryLimit: cfg.Manager.ImageBuild.HistoryLimit,
		Client:       mgr.GetClient(),
		Namespaces:   namespaces,
	})
}

func RegisterImageBuildDelete(mgr ctrl.Manager, deleteChan chan client.ObjectKey) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}, builder.WithPredicates(predicate.BlindDeletePredicate{})).
		Named("imagebuilddelete").
		Component("delete-broadcaster", component.DeleteBroadcaster(deleteChan)).
		ReconcileNotFound().
		Complete()
}
