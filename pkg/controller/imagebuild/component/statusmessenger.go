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

var publishContentType = "application/json"

type StatusMessage struct {
	Name          string            `json:"name"`
	Annotations   map[string]string `json:"annotations"`
	ObjectLink    string            `json:"objectLink"`
	PreviousPhase hephv1.Phase      `json:"previousPhase"`
	CurrentPhase  hephv1.Phase      `json:"currentPhase"`
	OccurredAt    time.Time         `json:"-"`

	// NOTE: think about adding ErrorMessage, ImageURLs and ImageSize
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
		log.V(1).Info("Aborting reconcile, messaging is not enabled")
		return ctrl.Result{}, nil
	}

	log.Info("Creating AMQP message publisher")
	publisher, err := amqp.NewPublisher(ctx.Log, c.cfg.AMQP.URL)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		log.V(1).Info("Closing message publisher")
		if err := publisher.Close(); err != nil {
			log.Error(err, "Failed to close message publisher")
		}

		log.V(1).Info("Message publisher closed")
	}()

	publishOpts := amqp.PublishOptions{
		ExchangeName: c.cfg.AMQP.Exchange,
		QueueName:    c.cfg.AMQP.Queue,
		ContentType:  publishContentType,
	}

	if ov := obj.Spec.AMQPOverrides; ov != nil {
		if ov.ExchangeName != "" {
			log.Info("Overriding target AMQP Exchange", "name", ov.ExchangeName)
			publishOpts.ExchangeName = ov.ExchangeName
		}

		if ov.QueueName != "" {
			log.Info("Overriding target AMQP Queue", "name", ov.QueueName)
			publishOpts.QueueName = ov.QueueName
		}
	}

	for _, transition := range obj.Status.Transitions {
		if transition.Processed {
			log.Info("Transition has been processed, skipping", "transition", transition)
			continue
		}

		log.Info("Processing phase transition", "from", transition.PreviousPhase, "to", transition.Phase)

		log.V(1).Info("Building object link")
		objLink, err := BuildObjectLink(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		msg := StatusMessage{
			Name:          obj.Name,
			Annotations:   obj.Annotations,
			ObjectLink:    objLink,
			PreviousPhase: transition.PreviousPhase,
			CurrentPhase:  transition.Phase,
			OccurredAt:    transition.OccurredAt.Time,
		}

		log.V(1).Info("Marshalling StatusMessage into JSON", "object", msg)
		content, err := json.Marshal(msg)
		if err != nil {
			return ctrl.Result{}, err
		}

		publishOpts.Body = content

		log.Info("Publishing transition message")
		if err = publisher.Publish(publishOpts); err != nil {
			return ctrl.Result{}, err
		}

		log.Info("Marking phase transition complete", "phase", transition.Phase)
		transition.Processed = true
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
