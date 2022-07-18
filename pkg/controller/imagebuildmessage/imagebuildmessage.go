package imagebuildmessage

import (
	"github.com/dominodatalab/controller-util/core"
	"github.com/newrelic/go-agent/v3/newrelic"
	ctrl "sigs.k8s.io/controller-runtime"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/controller/imagebuildmessage/component"
)

func Register(mgr ctrl.Manager, cfg config.Controller, nr *newrelic.Application) error {
	if !cfg.Messaging.Enabled {
		ctrl.Log.WithName("controller").WithName("imagebuildmessage").Info(
			"Aborting registration, messaging is not enabled",
		)
		return nil
	}

	return core.NewReconciler(mgr).
		For(&hephv1.ImageBuildMessage{}).
		Component("amqp-messenger", component.StatusMessenger(cfg.Messaging, nr)).
		ReconcileNotFound().
		Complete()
}
