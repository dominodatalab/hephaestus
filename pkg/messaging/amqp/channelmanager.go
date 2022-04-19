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

type channelManager struct {
	mu  sync.Mutex
	log logr.Logger

	url     string
	conn    *amqp.Connection
	channel *amqp.Channel

	shutdown chan struct{}
}

func NewChannelManager(log logr.Logger, url string) (*channelManager, error) {
	log.Info("Dialing AMQP server", "url", url)
	conn, ch, err := Dial(url)
	if err != nil {
		return nil, err
	}

	manager := &channelManager{
		log:     log.WithName("amqp.channel-manager"),
		url:     url,
		conn:    conn,
		channel: ch,
	}
	go manager.handleNotifications()

	return manager, nil
}

func (m *channelManager) Channel() *amqp.Channel {
	for m.channel.IsClosed() {
		time.Sleep(closedChannelLoopDelay)
	}

	return m.channel
}

func (m *channelManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.shutdown <- struct{}{}

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

func (m *channelManager) handleNotifications() {
	connCloses := m.conn.NotifyClose(make(chan *amqp.Error, 1))
	chanCloses := m.channel.NotifyClose(make(chan *amqp.Error, 1))
	chanCancels := m.channel.NotifyCancel(make(chan string, 1))

	// TODO: this design seems a little silly
	// 	it turns out that calling ...Passive() will close both the channel and connection
	//  so both need to be re-established. i think a better design would be to perform a
	//  series of checks inside reconnect() and (a) re-establish the connection if conn.IsClosed()
	//  or (b) just the channel when the former is still open.

	select {
	case err := <-connCloses:
		m.log.Error(err, "Connection closed, attempting full reconnect")
		m.reconnectWithRetry(true)
	case err := <-chanCloses:
		m.log.Error(err, "Channel closed, attempting to reconnect")
		m.reconnectWithRetry(true) // NOTE: changed to accommodate Passive() funcs behavior
	case msg := <-chanCancels:
		m.log.Error(errors.New(msg), "Channel canceled, attempting to reconnect")
		m.reconnectWithRetry(false)
	case <-m.shutdown:
		m.log.Info("Shutting down")
		return
	}

	m.log.Info("Successfully reconnected after close")
}

func (m *channelManager) reconnectWithRetry(full bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_ = retry.Do(
		func() error {
			return m.reconnect(full)
		},
		retry.OnRetry(func(n uint, err error) {
			m.log.Error(err, "Reconnect failed", "attempt", n)
		}),
		retry.Delay(reconnectDelay),
		retry.MaxDelay(reconnectMaxDelay),
		retry.DelayType(retry.BackOffDelay),
	)
}

func (m *channelManager) reconnect(full bool) error {
	if full {
		conn, ch, err := Dial(m.url)
		if err != nil {
			return err
		}

		_ = m.channel.Close()
		_ = m.conn.Close()

		m.conn = conn
		m.channel = ch
	} else {
		ch, err := m.conn.Channel()
		if err != nil {
			return err
		}

		_ = m.channel.Close()
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
