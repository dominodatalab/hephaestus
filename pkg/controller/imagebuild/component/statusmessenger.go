package component

import (
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/dominodatalab/controller-util/core"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
	"github.com/dominodatalab/hephaestus/pkg/messaging/amqp"
)

type StatusMessage struct {
	Name          string            `json:"name"`
	Annotations   map[string]string `json:"annotations"`
	ObjectLink    string            `json:"objectLink"`
	PreviousPhase hephv1.Phase      `json:"previousPhase"`
	CurrentPhase  hephv1.Phase      `json:"currentPhase"`
	OccurredAt    time.Time         `json:"occurredAt"`
}

type StatusMessengerComponent struct {
	cfg config.Messaging
}

func StatusMessenger(cfg config.Messaging) *StatusMessengerComponent {
	return &StatusMessengerComponent{
		cfg: cfg,
	}
}

func (c *StatusMessengerComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object.(*hephv1.ImageBuild)

	if !c.cfg.Enabled {
		log.V(1).Info("Messaging is not enabled, skipping")
		return ctrl.Result{}, nil
	}

	log.Info("Creating AMQP publisher")
	publisher, err := amqp.NewPublisher(ctx.Log, c.cfg.AMQP.URL)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer publisher.Close()

	log.Info("Processing status transitions", "count", len(obj.Status.Transitions))
	for _, tr := range obj.Status.Transitions {
		l := log.WithValues("from", tr.PreviousPhase, "to", tr.Phase)

		if tr.Processed {
			l.Info("Transition has been processed, skipping")
			continue
		}

		l.Info("Processing transition")

		l.V(1).Info("Building object link")
		objLink, err := BuildObjectLink(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		msg := StatusMessage{
			Name:          obj.Name,
			Annotations:   obj.Annotations,
			ObjectLink:    objLink,
			PreviousPhase: tr.PreviousPhase,
			CurrentPhase:  tr.Phase,
			OccurredAt:    tr.OccurredAt.Time,
		}

		l.V(1).Info("Marshalling StatusMessage into JSON")
		content, err := json.Marshal(msg)
		if err != nil {
			return ctrl.Result{}, err
		}

		opts := amqp.PublishOptions{
			ExchangeName: c.cfg.AMQP.Exchange,
			QueueName:    c.cfg.AMQP.Queue,
			Body:         content,
			ContentType:  "application/json",
		}

		l.Info("Publishing status")
		if err = publisher.Publish(opts); err != nil {
			return ctrl.Result{}, err
		}

		l.Info("Updating status transition")
		tr.Processed = true
		if err := ctx.Client.Status().Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func BuildObjectLink(ctx *core.Context) (string, error) {
	gvk, err := apiutil.GVKForObject(ctx.Object, ctx.Scheme)
	if err != nil {
		return "", err
	}

	link := path.Join(
		"/apis",
		gvk.GroupVersion().String(),
		"namespaces",
		ctx.Object.GetNamespace(),
		strings.ToLower(gvk.Kind),
		ctx.Object.GetName(),
	)
	return link, nil
}
