package amqp

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/go-logr/logr"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	connectionTimeout      = 10 * time.Second
	reconnectDelay         = 1 * time.Second
	reconnectMaxDelay      = 30 * time.Second
	closedChannelLoopDelay = 100 * time.Millisecond
)

type ChannelManager interface {
	Channel() *amqp.Channel
	Close() error
}

type Manager struct {
	mu  sync.Mutex
	log logr.Logger

	url     string
	conn    *amqp.Connection
	channel *amqp.Channel

	shutdown chan struct{}
}

func NewChannelManager(log logr.Logger, url string) (*Manager, error) {
	log = log.WithName("amqp.channel-manager")

	log.V(1).Info("Dialing server", "url", url)
	conn, ch, err := Dial(url)
	if err != nil {
		return nil, err
	}

	manager := &Manager{
		log:      log,
		url:      url,
		conn:     conn,
		channel:  ch,
		shutdown: make(chan struct{}),
	}
	go manager.handleNotifications()

	return manager, nil
}

func (m *Manager) Channel() *amqp.Channel {
	for m.channel.IsClosed() {
		time.Sleep(closedChannelLoopDelay)
	}

	return m.channel
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.V(1).Info("Sending shutdown signal")
	m.shutdown <- struct{}{}
	m.log.V(1).Info("Shutdown signal sent")

	if !m.channel.IsClosed() {
		if err := m.channel.Close(); err != nil {
			return err
		}
	}
	if !m.conn.IsClosed() {
		if err := m.conn.Close(); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) handleNotifications() {
	connCloses := m.conn.NotifyClose(make(chan *amqp.Error, 1))
	chanCloses := m.channel.NotifyClose(make(chan *amqp.Error, 1))
	chanCancels := m.channel.NotifyCancel(make(chan string, 1))

	select {
	case err := <-connCloses:
		m.log.Error(err, "Connection closed")
	case err := <-chanCloses:
		m.log.Error(err, "Channel closed")
	case msg := <-chanCancels:
		m.log.Error(errors.New(msg), "Channel canceled")
	case <-m.shutdown:
		m.log.V(1).Info("Shutting down")
		return
	}

	select {
	case <-m.shutdown:
		m.log.V(1).Info("Shutting down")
	default:
		m.log.Info("Attemtping to reconnect")
		m.reconnectWithRetry()
		m.log.Info("Successfully reconnected after close")
	}
}

func (m *Manager) reconnectWithRetry() {
	m.mu.Lock()
	defer m.mu.Unlock()

	_ = retry.Do(
		func() error {
			return m.reconnect()
		},
		retry.OnRetry(func(n uint, err error) {
			m.log.Error(err, "Reconnect failed", "attempt", n)
		}),
		retry.Delay(reconnectDelay),
		retry.MaxDelay(reconnectMaxDelay),
		retry.DelayType(retry.BackOffDelay),
	)
}

func (m *Manager) reconnect() error {
	_ = m.channel.Close()

	if m.conn.IsClosed() {
		_ = m.conn.Close()

		conn, ch, err := Dial(m.url)
		if err != nil {
			return err
		}

		m.conn = conn
		m.channel = ch
	} else if m.channel.IsClosed() {
		ch, err := m.conn.Channel()
		if err != nil {
			return err
		}

		m.channel = ch
	}

	go m.handleNotifications()

	return nil
}

func Dial(url string) (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{Dial: amqp.DefaultDial(connectionTimeout)})
	if err != nil {
		return nil, nil, fmt.Errorf("dialing amqp url %q failed: %w", url, err)
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, fmt.Errorf("opening channel failed: %w", err)
	}

	return conn, ch, nil
}
