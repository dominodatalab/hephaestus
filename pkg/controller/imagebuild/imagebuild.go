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

var ch = make(chan client.ObjectKey)

func Register(mgr ctrl.Manager, cfg config.Controller, pool worker.Pool, nr *newrelic.Application) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}).
		Component("build-dispatcher", component.BuildDispatcher(cfg.Buildkit, pool, nr, ch)).
		WithControllerOptions(controller.Options{MaxConcurrentReconciles: cfg.ImageBuildMaxConcurrency}).
		WithWebhooks().
		Complete()
}

func RegisterImageBuildStatus(mgr ctrl.Manager, cfg config.Controller, nr *newrelic.Application) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}, builder.WithPredicates(predicate.UnprocessedTransitionsPredicate{})).
		Named("imagebuildstatus").
		Component("status-messenger", component.StatusMessenger(cfg.Messaging, nr)).
		Complete()
}

func RegisterImageBuildDelete(mgr ctrl.Manager) error {
	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuild{}, builder.WithPredicates(predicate.BlindDeletePredicate{})).
		Named("imagebuilddelete").
		Component("delete-broadcaster", component.DeleteBroadcaster(ch)).
		ReconcileNotFound().
		Complete()
}
