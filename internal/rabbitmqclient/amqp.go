package rabbitmqclient

import (
	"crypto/tls"

	amqp "github.com/rabbitmq/amqp091-go"
)

// dialer, connection and channel are the parts of amqp091 the client uses,
// so tests can replace the broker with fakes.
type dialer interface {
	dial() (connection, error)
}

type connection interface {
	Channel() (channel, error)
	NotifyClose(chan *amqp.Error) chan *amqp.Error
	Close() error
}

type channel interface {
	ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	ExchangeDeclarePassive(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error)
	QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error
	Qos(prefetchCount, prefetchSize int, global bool) error
	Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	NotifyClose(chan *amqp.Error) chan *amqp.Error
	NotifyCancel(chan string) chan string
}

// amqpDialer connects to the configured broker with amqp091.
type amqpDialer struct {
	cfg Config
	tls *tls.Config
}

func (d amqpDialer) dial() (connection, error) {
	uri := amqp.URI{
		Scheme:   "amqp",
		Host:     d.cfg.Host,
		Port:     d.cfg.Port,
		Username: d.cfg.Username,
		Password: d.cfg.Password,
		Vhost:    d.cfg.VHost,
	}
	if d.tls != nil {
		uri.Scheme = "amqps"
	}
	props := amqp.NewConnectionProperties()
	props.SetClientConnectionName(connectionName)
	conn, err := amqp.DialConfig(uri.String(), amqp.Config{
		Heartbeat:       heartbeat,
		TLSClientConfig: d.tls,
		Properties:      props,
		Dial:            amqp.DefaultDial(dialTimeout),
	})
	if err != nil {
		return nil, err
	}
	return amqpConn{conn}, nil
}

// amqpConn adapts *amqp.Connection, whose Channel returns *amqp.Channel.
type amqpConn struct{ *amqp.Connection }

func (c amqpConn) Channel() (channel, error) {
	ch, err := c.Connection.Channel()
	if err != nil {
		return nil, err
	}
	return ch, nil
}
