package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Reconnect backoff, as in tropobeat's RabbitMQ client.
const (
	reconnectFirst = time.Second
	reconnectMax   = 30 * time.Second
)

// publisher publishes one report body.
type publisher interface {
	connect() error
	publish(ctx context.Context, body []byte, ts time.Time) error
	close()
}

// amqpPublisher publishes to a RabbitMQ exchange.
type amqpPublisher struct {
	uri, exchange, kind, key string
	declare                  bool

	conn *amqp.Connection
	ch   *amqp.Channel
}

func (p *amqpPublisher) connect() error {
	conn, err := amqp.Dial(p.uri)
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}
	if p.declare {
		if err := ch.ExchangeDeclare(p.exchange, p.kind, true, false, false, false, nil); err != nil {
			conn.Close()
			return fmt.Errorf("declaring exchange %s: %w", p.exchange, err)
		}
	}
	p.conn, p.ch = conn, ch
	return nil
}

func (p *amqpPublisher) publish(ctx context.Context, body []byte, ts time.Time) error {
	if p.ch == nil {
		return errors.New("not connected")
	}
	return p.ch.PublishWithContext(ctx, p.exchange, p.key, false, false, amqp.Publishing{
		ContentType: "application/json",
		Timestamp:   ts,
		Body:        body,
	})
}

func (p *amqpPublisher) close() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}

// reconnect closes the publisher and connects again, waiting 1s, then
// doubling up to 30s (with jitter) between attempts. It returns false when
// ctx ends first.
func reconnect(ctx context.Context, p publisher, sleep func(context.Context, time.Duration) bool) bool {
	p.close()
	wait := reconnectFirst
	for attempt := 1; ; attempt++ {
		jitter := time.Duration(rand.Int64N(int64(wait) / 4))
		if !sleep(ctx, wait+jitter) {
			return false
		}
		err := p.connect()
		if err == nil {
			log.Printf("Reconnected after %d attempt(s)", attempt)
			return true
		}
		log.Printf("Reconnect attempt %d failed: %v", attempt, err)
		wait = min(2*wait, reconnectMax)
	}
}

// sleepCtx waits for d, or returns false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
