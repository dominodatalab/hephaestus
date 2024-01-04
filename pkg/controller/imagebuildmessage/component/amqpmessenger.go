package component

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/distribution/reference"
	amqpclient "github.com/dominodatalab/amqp-client"
	"github.com/dominodatalab/controller-util/core"
	"github.com/newrelic/go-agent/v3/newrelic"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
	"github.com/dominodatalab/hephaestus/pkg/config"
)

const publishContentType = "application/json"

type AMQPMessengerComponent struct {
	cfg      config.Messaging
	newRelic *newrelic.Application
}

func StatusMessenger(cfg config.Messaging, nr *newrelic.Application) *AMQPMessengerComponent {
	return &AMQPMessengerComponent{
		cfg:      cfg,
		newRelic: nr,
	}
}

func (c *AMQPMessengerComponent) Initialize(_ *core.Context, bldr *ctrl.Builder) error {
	bldr.Watches(
		&hephv1.ImageBuild{},
		&handler.EnqueueRequestForObject{},
		builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			UpdateFunc:  func(event.UpdateEvent) bool { return true },
			GenericFunc: func(event.GenericEvent) bool { return false },
		}),
	)

	return nil
}

//nolint:maintidx
func (c *AMQPMessengerComponent) Reconcile(ctx *core.Context) (ctrl.Result, error) {
	log := ctx.Log
	obj := ctx.Object
	objKey := client.ObjectKey{Name: obj.GetName(), Namespace: obj.GetNamespace()}

	txn := c.newRelic.StartTransaction("StatusMessengerComponent.Reconcile")
	txn.AddAttribute("imagebuild", objKey.String())
	txn.AddAttribute("url", c.cfg.AMQP.URL)
	defer txn.End()

	ib := &hephv1.ImageBuild{}
	if err := ctx.Client.Get(ctx, objKey, ib); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Aborting reconcile, ImageBuild does not exist")
			txn.Ignore()

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	amqpMsg := amqpclient.SimpleMessage{
		ExchangeName: c.cfg.AMQP.Exchange,
		QueueName:    c.cfg.AMQP.Queue,
		ContentType:  publishContentType,
	}

	if ov := ib.Spec.AMQPOverrides; ov != nil {
		if ov.ExchangeName != "" {
			log.Info("Overriding target AMQP Exchange", "name", ov.ExchangeName)
			amqpMsg.ExchangeName = ov.ExchangeName
		}

		if ov.QueueName != "" {
			log.Info("Overriding target AMQP Queue", "name", ov.QueueName)
			amqpMsg.QueueName = ov.QueueName
		}
	}
	txn.AddAttribute("queue", amqpMsg.QueueName)
	txn.AddAttribute("exchange", amqpMsg.ExchangeName)

	var ibm hephv1.ImageBuildMessage
	if err := ctx.Client.Get(ctx, objKey, &ibm); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		log.Info("Creating resource, ImageBuildMessage does not exist")
		u, _ := url.Parse(c.cfg.AMQP.URL)
		ibm = hephv1.ImageBuildMessage{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ib.Name,
				Namespace: ib.Namespace,
			},
			Spec: hephv1.ImageBuildMessageSpec{
				AMQP: hephv1.ImageBuildMessageAMQPConnection{
					URI:      u.Redacted(),
					Queue:    amqpMsg.QueueName,
					Exchange: amqpMsg.ExchangeName,
				},
			},
		}

		if err = controllerutil.SetOwnerReference(ib, &ibm, ctx.Scheme); err != nil {
			return ctrl.Result{}, err
		}

		if err = ctx.Client.Create(ctx, &ibm); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("Creating AMQP message publisher")
	connectSeg := txn.StartSegment("broker-connect")
	amqpClient, err := amqpclient.NewSimpleClient(log, c.cfg.AMQP.URL)
	if err != nil {
		txn.NoticeError(newrelic.Error{
			Message: err.Error(),
			Class:   "BrokerConnectError",
		})
		return ctrl.Result{}, err
	}
	connectSeg.End()

	defer func() {
		log.V(1).Info("Closing message publisher")
		if err := amqpClient.Close(); err != nil {
			log.Error(err, "Failed to close message publisher")
		}

		log.V(1).Info("Message publisher closed")
	}()

	recordMap := make(map[hephv1.Phase]hephv1.ImageBuildMessageRecord)
	for _, record := range ibm.Status.AMQPSentMessages {
		recordMap[record.Message.CurrentPhase] = record
	}

	for _, trans := range ib.Status.Transitions {
		if record, ok := recordMap[trans.Phase]; ok {
			log.Info("Transition has been processed, skipping", "phase", record.Message.CurrentPhase)
			continue
		}
		log.Info("Processing phase transition", "from", trans.PreviousPhase, "to", trans.Phase)

		transitionSeg := txn.StartSegment(fmt.Sprintf("transition-to-%s", strings.ToLower(string(trans.Phase))))
		transitionSeg.AddAttribute("previous-phase", string(trans.PreviousPhase))

		log.V(1).Info("Building object link")
		objLink, err := BuildObjectLink(ib, ctx.Scheme)
		if err != nil {
			txn.NoticeError(newrelic.Error{
				Message: err.Error(),
				Class:   "ObjectLinkError",
			})
			return ctrl.Result{}, err
		}

		message := hephv1.ImageBuildStatusTransitionMessage{
			Name:          ib.Name,
			Annotations:   ib.Annotations,
			ObjectLink:    objLink,
			PreviousPhase: trans.PreviousPhase,
			CurrentPhase:  trans.Phase,
			OccurredAt:    trans.OccurredAt,
		}

		switch trans.Phase {
		case hephv1.PhaseSucceeded:
			var images []string
			for _, image := range ib.Spec.Images {
				named, err := reference.ParseNormalizedNamed(image)
				if err != nil {
					txn.NoticeError(newrelic.Error{
						Message: err.Error(),
						Class:   "ParseImageError",
					})
					return ctrl.Result{}, fmt.Errorf("parsing image name %q failed: %w", image, err)
				}

				images = append(images, reference.TagNameOnly(named).String())
			}
			message.ImageURLs = images
			message.Annotations["imagebuilder.dominodatalab.com/image-size"] = "123456789"
			annotations := obj.GetAnnotations()
			log.Info("Hello2 annotations", "annotations", annotations)
		case hephv1.PhaseFailed:
			if ib.Status.Conditions == nil {
				return ctrl.Result{Requeue: true}, nil
			}

			for _, condition := range ib.Status.Conditions {
				if condition.Status == metav1.ConditionFalse {
					message.ErrorMessage = condition.Message
				}
			}
		}

		log.V(1).Info("Marshalling ImageBuildStatusTransitionMessage into JSON", "message", message)
		content, err := json.Marshal(message)
		if err != nil {
			txn.NoticeError(newrelic.Error{
				Message: err.Error(),
				Class:   "StatusMessageMarshalError",
			})
			return ctrl.Result{}, err
		}
		amqpMsg.Body = content

		log.Info("Publishing transition message")
		if err = amqpClient.Publish(ctx, amqpMsg); err != nil {
			txn.NoticeError(newrelic.Error{
				Message: err.Error(),
				Class:   "MessagePublishError",
			})
			return ctrl.Result{}, err
		}

		ibm.Status.AMQPSentMessages = append(ibm.Status.AMQPSentMessages, hephv1.ImageBuildMessageRecord{
			SentAt:  metav1.Time{Time: time.Now()},
			Message: message,
		})

		log.Info("Updating sent AMQP messages status", "phase", message.CurrentPhase)
		if err = ctx.Client.Status().Update(ctx, &ibm); err != nil {
			txn.NoticeError(newrelic.Error{
				Message: err.Error(),
				Class:   "UpdateStatusError",
			})
			return ctrl.Result{}, err
		}

		transitionSeg.End()
	}

	return ctrl.Result{}, nil
}

func BuildObjectLink(obj client.Object, scheme *runtime.Scheme) (string, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return "", err
	}

	link := path.Join(
		"/apis",
		gvk.GroupVersion().String(),
		"namespaces",
		obj.GetNamespace(),
		strings.ToLower(gvk.Kind),
		obj.GetName(),
	)
	return link, nil
}
