package amqp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	AppID             = "hephaestus"
	MandatoryDelivery = true
	ImmediateDelivery = false

	ExchangeType       = "direct"
	ExchangeDurable    = true
	ExchangeAutoDelete = false
	ExchangeInternal   = false
	ExchangeNoWait     = false

	QueueDurable    = true
	QueueAutoDelete = false
	QueueExclusive  = false
	QueueNoWait     = false
)

type PublishOptions struct {
	ExchangeName string
	QueueName    string
	ContentType  string
	Body         []byte
}

type Publisher interface {
	Publish(PublishOptions) error
	Close() error
}

func IsNotFound(err error) bool {
	var ae *amqp.Error
	return errors.As(err, &ae) && strings.HasPrefix(ae.Reason, "NOT_FOUND")
}

type publisher struct {
	log     logr.Logger
	manager ChannelManager
}

func NewPublisher(log logr.Logger, url string) (*publisher, error) {
	manager, err := NewChannelManager(log, url)
	if err != nil {
		return nil, fmt.Errorf("cannot create channel manager: %w", err)
	}

	return &publisher{
		log:     log.WithName("amqp.publisher"),
		manager: manager,
	}, nil
}

func (p *publisher) Publish(opts PublishOptions) error {
	if err := p.ensureExchange(opts.ExchangeName); err != nil {
		return err
	}
	if err := p.ensureQueue(opts.ExchangeName, opts.QueueName); err != nil {
		return err
	}

	message := amqp.Publishing{
		AppId:        AppID,
		Timestamp:    time.Now(),
		DeliveryMode: amqp.Persistent,
		ContentType:  opts.ContentType,
		Body:         opts.Body,
	}

	p.log.Info("Publishing message", "contents", message)
	err := p.manager.Channel().Publish(opts.ExchangeName, opts.QueueName, MandatoryDelivery, ImmediateDelivery, message)
	if err != nil {
		return fmt.Errorf("message publishing failed: %w", err)
	}

	return nil
}

func (p *publisher) Close() error {
	return p.manager.Close()
}

func (p *publisher) ensureExchange(exchange string) error {
	if exchange != "" {
		err := p.manager.Channel().ExchangeDeclarePassive(
			exchange,
			ExchangeType,
			ExchangeDurable,
			ExchangeAutoDelete,
			ExchangeInternal,
			ExchangeNoWait,
			nil,
		)

		if err != nil {
			if !IsNotFound(err) {
				return err
			}

			err = p.manager.Channel().ExchangeDeclare(
				exchange,
				ExchangeType,
				ExchangeDurable,
				ExchangeAutoDelete,
				ExchangeInternal,
				ExchangeNoWait,
				nil,
			)
			if err != nil {
				return fmt.Errorf("cannot declare exchange %q: %w", exchange, err)
			}
		}
	}

	return nil
}

func (p *publisher) ensureQueue(exchange, queue string) error {
	if queue != "" {
		_, err := p.manager.Channel().QueueDeclarePassive(
			queue,
			QueueDurable,
			QueueAutoDelete,
			QueueExclusive,
			QueueNoWait,
			nil,
		)

		if err != nil {
			if !IsNotFound(err) {
				return err
			}

			_, err = p.manager.Channel().QueueDeclare(
				queue,
				QueueDurable,
				QueueAutoDelete,
				QueueExclusive,
				QueueNoWait,
				nil,
			)
			if err != nil {
				return fmt.Errorf("cannot declare queue %q: %w", queue, err)
			}
		}

		if exchange != "" {
			err = p.manager.Channel().QueueBind(queue, queue, exchange, QueueNoWait, nil)
			if err != nil {
				return fmt.Errorf("cannot bind queue %q: %w", queue, err)
			}
		}
	}

	return nil
}
