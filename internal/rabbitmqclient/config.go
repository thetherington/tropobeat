package rabbitmqclient

import (
	"errors"
	"fmt"
	"time"
)

// Exchange declare modes (Config.ExchangeDeclare).
const (
	// DeclarePassive only checks that the exchange exists, with the
	// configured type and durability. Tropobeat never creates or changes it.
	// This is the default, because the demod exchange belongs to inSITE.
	DeclarePassive = "passive"
	// DeclareActive creates the exchange if it is missing. Meant for
	// development and replay setups.
	DeclareActive = "active"
)

// SSLConfig enables TLS (amqps) for the broker connection.
type SSLConfig struct {
	Enabled bool `config:"enabled"`
	// CACert is a PEM CA certificate file to trust, in addition to the
	// system pool.
	CACert string `config:"ca_cert"`
	// Insecure skips certificate verification. For testing only.
	Insecure bool `config:"insecure"`
}

// Config holds the RabbitMQ connection, topology and reconnect settings
// (the tropobeat.rabbitmq section). The key names match routelogbeat's
// rabbitmq_client section.
type Config struct {
	// Enabled turns the demod stream on. Off by default.
	Enabled bool `config:"enabled"`

	Host     string    `config:"host"`
	Port     int       `config:"port"`
	VHost    string    `config:"vhost"`
	Username string    `config:"username"`
	Password string    `config:"password"`
	SSL      SSLConfig `config:"ssl"`

	// Exchange is the exchange the demod reports are published to.
	Exchange     string `config:"exchange"`
	ExchangeType string `config:"exchange_type"`
	// ExchangeDeclare is DeclarePassive (default) or DeclareActive.
	ExchangeDeclare string `config:"exchange_declare"`
	// Durable applies to the exchange (when declared actively, or checked
	// passively) and to tropobeat's queue.
	Durable bool `config:"durable"`
	// Queue is tropobeat's own queue, bound to Exchange with RoutingKey, so
	// tropobeat gets a copy of every report without taking messages from
	// other consumers.
	Queue      string `config:"queue"`
	RoutingKey string `config:"routing_key"`

	// Prefetch is how many unacknowledged reports the broker sends ahead.
	Prefetch int `config:"prefetch"`
	// MessageTTL and MaxLength bound the queue (x-message-ttl and
	// x-max-length, dropping the oldest reports first), so a stopped
	// tropobeat can't build up a backlog of stale reports.
	MessageTTL time.Duration `config:"message_ttl"`
	MaxLength  int           `config:"max_length"`

	// ReconnectInterval is the first retry delay after a failure. It doubles
	// on each further failure up to ReconnectMax, and resets once a
	// connection has stayed up for a minute.
	ReconnectInterval time.Duration `config:"reconnect_interval"`
	ReconnectMax      time.Duration `config:"reconnect_max"`
}

// DefaultConfig is the default RabbitMQ configuration. There is
// deliberately no default password.
var DefaultConfig = Config{
	Enabled:           false,
	Host:              "localhost",
	Port:              8910,
	VHost:             "/",
	Username:          "insite",
	Exchange:          "x.tropobeat.demod",
	ExchangeType:      "direct",
	ExchangeDeclare:   DeclarePassive,
	Durable:           true,
	Queue:             "q.tropobeat.demod",
	RoutingKey:        "demod.metrics",
	Prefetch:          500,
	MessageTTL:        5 * time.Minute,
	MaxLength:         50000,
	ReconnectInterval: time.Second,
	ReconnectMax:      30 * time.Second,
}

// Validate checks the configuration. A disabled configuration is always
// valid.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	var errs []error
	req := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("rabbitmq.%s is required", name))
		}
	}
	req("host", c.Host)
	req("username", c.Username)
	req("password", c.Password)
	req("exchange", c.Exchange)
	req("exchange_type", c.ExchangeType)
	req("queue", c.Queue)
	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("rabbitmq.port %d is out of range", c.Port))
	}
	if c.ExchangeDeclare != DeclarePassive && c.ExchangeDeclare != DeclareActive {
		errs = append(errs, fmt.Errorf("rabbitmq.exchange_declare must be %q or %q, got %q", DeclarePassive, DeclareActive, c.ExchangeDeclare))
	}
	if c.Prefetch <= 0 {
		errs = append(errs, errors.New("rabbitmq.prefetch must be positive"))
	}
	if c.MessageTTL < 0 || c.MaxLength < 0 {
		errs = append(errs, errors.New("rabbitmq.message_ttl and rabbitmq.max_length must not be negative (0 means unlimited)"))
	}
	if c.ReconnectInterval <= 0 || c.ReconnectMax < c.ReconnectInterval {
		errs = append(errs, errors.New("rabbitmq.reconnect_interval must be positive and no greater than reconnect_max"))
	}
	return errors.Join(errs...)
}

// Address returns the broker as "host:port/vhost" for logging. It never
// includes credentials.
func (c Config) Address() string {
	return fmt.Sprintf("%s:%d%s", c.Host, c.Port, c.VHost)
}
