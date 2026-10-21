// Package rabbitmqclient consumes the demod report stream from RabbitMQ. It
// declares tropobeat's own bounded queue, binds it to the demod exchange,
// and delivers every message on one channel that stays open across broker
// restarts and reconnects. Decoding and acknowledging are left to the
// caller.
//
// It is based on routelogbeat's rabbitmqclient, reworked so that a single
// supervisor goroutine owns the connection:
//
//	Consume(ctx)                         returns `out`, starts supervise
//	  supervise(ctx, out)                [defer close(out)]
//	    loop:
//	      session(ctx, out)              one connection's lifetime:
//	        dial -> channel -> exchange (passive or active) -> queue (TTL,
//	        max length) -> bind -> Qos(prefetch) -> Consume(manual ack)
//	        forward deliveries to out until one of:
//	          ctx done                   -> return, supervise exits
//	          connection closed          ┐
//	          channel closed             │ return error; supervise logs it,
//	          consumer cancelled         │ waits (backoff with jitter) and
//	          delivery stream closed     ┘ starts a new session
//	      backoff doubles per failure up to reconnect_max, and resets once
//	      a session has stayed up for stableAfter
//
// Because every failure, including a channel-only close (a precondition
// failure, a deleted queue or a broker cancel), ends the whole session, the
// next session always starts from a fresh connection and channel. No state
// is shared between goroutines except the counters.
package rabbitmqclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// consumerTag identifies tropobeat's consumer on its queue.
	consumerTag = "tropobeat"
	// connectionName is shown in the RabbitMQ management UI.
	connectionName = "tropobeat"
	// stableAfter is how long a session must stay up before the reconnect
	// backoff resets to reconnect_interval.
	stableAfter = time.Minute
	// heartbeat and dialTimeout are the AMQP connection settings.
	heartbeat   = 10 * time.Second
	dialTimeout = 30 * time.Second
)

// Client consumes tropobeat's demod queue. Create it with New, start it
// with Consume and stop it with Close (or by cancelling Consume's context).
type Client struct {
	cfg    Config
	logger *logp.Logger
	dial   dialer

	cancel     context.CancelFunc
	wg         sync.WaitGroup
	closeOnce  sync.Once
	started    atomic.Bool
	sessions   atomic.Int64
	reconnects atomic.Int64
}

// New validates cfg and returns a client. It does not connect: the first
// connection is made by Consume, so start-up never blocks on the broker.
func New(cfg Config, logger *logp.Logger) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsCfg, err := tlsConfig(cfg.SSL)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, logger: logger, dial: amqpDialer{cfg: cfg, tls: tlsCfg}}, nil
}

// Consume starts consuming and returns the delivery channel. The channel
// stays open across reconnects and is closed only when ctx is cancelled or
// Close is called. Each delivery must be acknowledged (Ack) or rejected
// (Nack) by the caller. Consume may be called only once.
func (c *Client) Consume(ctx context.Context) <-chan amqp.Delivery {
	out := make(chan amqp.Delivery)
	if !c.started.CompareAndSwap(false, true) {
		close(out)
		return out
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.wg.Add(1)
	go c.supervise(ctx, out)
	return out
}

// Reconnects returns how many times the client has connected again after
// its first session.
func (c *Client) Reconnects() int64 { return c.reconnects.Load() }

// Close stops consuming, closes the connection and waits for the
// supervisor to exit. It is safe to call more than once, and before
// Consume.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		c.wg.Wait()
	})
}

// supervise runs sessions until ctx is cancelled, backing off between
// failed ones.
func (c *Client) supervise(ctx context.Context, out chan<- amqp.Delivery) {
	defer c.wg.Done()
	defer close(out)

	backoff := c.cfg.ReconnectInterval
	for {
		start := time.Now()
		err := c.session(ctx, out)
		if ctx.Err() != nil {
			return
		}
		// A session that stayed up long enough was a working connection
		// that dropped, so start again from the shortest delay.
		if time.Since(start) >= stableAfter {
			backoff = c.cfg.ReconnectInterval
		}
		wait := jitter(backoff)
		c.logger.Warnf("RabbitMQ %s: %v; reconnecting in %s", c.cfg.Address(), c.redact(err), wait.Round(time.Millisecond))
		if !sleep(ctx, wait) {
			return
		}
		backoff = nextBackoff(backoff, c.cfg.ReconnectMax)
	}
}

// session runs one connection: it connects, sets up the topology and
// forwards deliveries to out until the connection, channel or consumer
// ends (an error) or ctx is cancelled (nil).
func (c *Client) session(ctx context.Context, out chan<- amqp.Delivery) error {
	if c.sessions.Add(1) > 1 {
		c.reconnects.Add(1)
	}

	conn, err := c.dial.dial()
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	// Closing the connection also closes its channel; unacknowledged
	// deliveries go back to the queue.
	defer conn.Close()
	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("opening channel: %w", err)
	}
	chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))
	cancelled := ch.NotifyCancel(make(chan string, 1))

	if err := c.setup(ch); err != nil {
		return err
	}
	deliveries, err := ch.Consume(c.cfg.Queue, consumerTag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consuming from queue %s: %w", c.cfg.Queue, err)
	}
	c.logger.Infof("RabbitMQ %s: consuming queue %s (exchange %s, routing key %q, prefetch %d)",
		c.cfg.Address(), c.cfg.Queue, c.cfg.Exchange, c.cfg.RoutingKey, c.cfg.Prefetch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-connClosed:
			return fmt.Errorf("connection closed: %v", e)
		case e := <-chClosed:
			return fmt.Errorf("channel closed: %v", e)
		case tag := <-cancelled:
			return fmt.Errorf("consumer %s cancelled by the broker (queue deleted?)", tag)
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("delivery stream closed")
			}
			select {
			case out <- d:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// setup declares (or checks) the exchange, declares tropobeat's bounded
// queue, binds it and sets the prefetch.
func (c *Client) setup(ch channel) error {
	cfg := c.cfg
	if cfg.ExchangeDeclare == DeclareActive {
		if err := ch.ExchangeDeclare(cfg.Exchange, cfg.ExchangeType, cfg.Durable, false, false, false, nil); err != nil {
			return fmt.Errorf("declaring exchange %s: %w", cfg.Exchange, err)
		}
	} else if err := ch.ExchangeDeclarePassive(cfg.Exchange, cfg.ExchangeType, cfg.Durable, false, false, false, nil); err != nil {
		return fmt.Errorf("checking exchange %s (exchange_declare: passive; it must already exist with type %s, durable %v): %w",
			cfg.Exchange, cfg.ExchangeType, cfg.Durable, err)
	}
	if _, err := ch.QueueDeclare(cfg.Queue, cfg.Durable, false, false, false, queueArgs(cfg)); err != nil {
		return fmt.Errorf("declaring queue %s (if its settings changed, delete the queue so it can be recreated): %w", cfg.Queue, err)
	}
	if err := ch.QueueBind(cfg.Queue, cfg.RoutingKey, cfg.Exchange, false, nil); err != nil {
		return fmt.Errorf("binding queue %s to exchange %s: %w", cfg.Queue, cfg.Exchange, err)
	}
	if err := ch.Qos(cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("setting prefetch: %w", err)
	}
	return nil
}

// queueArgs bounds the queue: reports older than message_ttl expire, and
// beyond max_length the oldest are dropped. Zero leaves a bound off.
func queueArgs(cfg Config) amqp.Table {
	args := amqp.Table{}
	if cfg.MessageTTL > 0 {
		args["x-message-ttl"] = cfg.MessageTTL.Milliseconds()
	}
	if cfg.MaxLength > 0 {
		args["x-max-length"] = int64(cfg.MaxLength)
		args["x-overflow"] = "drop-head"
	}
	return args
}

// redact removes the password from err's message, in case a library error
// ever includes the connection URL.
func (c *Client) redact(err error) string {
	s := fmt.Sprint(err)
	if c.cfg.Password != "" {
		s = strings.ReplaceAll(s, c.cfg.Password, "xxxxx")
	}
	return s
}

// nextBackoff doubles d, up to limit.
func nextBackoff(d, limit time.Duration) time.Duration {
	if d *= 2; d > limit {
		return limit
	}
	return d
}

// jitter returns a random delay between d/2 and d, so many clients don't
// reconnect in step after a broker outage.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + rand.N(d-half+1)
}

// sleep waits for d, returning false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// tlsConfig builds the TLS settings for amqps, or nil when SSL is off.
func tlsConfig(s SSLConfig) (*tls.Config, error) {
	if !s.Enabled {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.Insecure} //nolint:gosec // opt-in, for testing only
	if s.CACert != "" {
		pem, err := os.ReadFile(s.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading rabbitmq.ssl.ca_cert: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("rabbitmq.ssl.ca_cert %s: no PEM certificates found", s.CACert)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
