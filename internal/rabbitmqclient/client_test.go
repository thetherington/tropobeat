package rabbitmqclient

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	amqp "github.com/rabbitmq/amqp091-go"
)

// fakeBroker hands out fake connections. Each connection has one channel
// whose deliveries and close/cancel notifications the test drives.
type fakeBroker struct {
	mu       sync.Mutex
	failDial int // dial attempts that fail before one succeeds
	conns    []*fakeConn
	connCh   chan *fakeConn // receives each connection once it is consuming
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{connCh: make(chan *fakeConn, 10)}
}

func (b *fakeBroker) dial() (connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failDial > 0 {
		b.failDial--
		return nil, errors.New("connection refused")
	}
	c := &fakeConn{broker: b, ch: &fakeChannel{deliveries: make(chan amqp.Delivery, 10)}}
	c.ch.conn = c
	b.conns = append(b.conns, c)
	return c, nil
}

type fakeConn struct {
	broker *fakeBroker
	ch     *fakeChannel
	mu     sync.Mutex
	notify chan *amqp.Error
	closed bool
}

func (c *fakeConn) Channel() (channel, error) { return c.ch, nil }

func (c *fakeConn) NotifyClose(ch chan *amqp.Error) chan *amqp.Error {
	c.notify = ch
	return ch
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type fakeChannel struct {
	conn         *fakeConn
	deliveries   chan amqp.Delivery
	closeNotify  chan *amqp.Error
	cancelNotify chan string

	exchangeActive, exchangePassive bool
	queueArgs                       amqp.Table
	bindKey                         string
	prefetch                        int
}

func (f *fakeChannel) ExchangeDeclare(string, string, bool, bool, bool, bool, amqp.Table) error {
	f.exchangeActive = true
	return nil
}

func (f *fakeChannel) ExchangeDeclarePassive(string, string, bool, bool, bool, bool, amqp.Table) error {
	f.exchangePassive = true
	return nil
}

func (f *fakeChannel) QueueDeclare(name string, _, _, _, _ bool, args amqp.Table) (amqp.Queue, error) {
	f.queueArgs = args
	return amqp.Queue{Name: name}, nil
}

func (f *fakeChannel) QueueBind(_, key, _ string, _ bool, _ amqp.Table) error {
	f.bindKey = key
	return nil
}

func (f *fakeChannel) Qos(n, _ int, _ bool) error { f.prefetch = n; return nil }

func (f *fakeChannel) Consume(string, string, bool, bool, bool, bool, amqp.Table) (<-chan amqp.Delivery, error) {
	f.conn.broker.connCh <- f.conn
	return f.deliveries, nil
}

func (f *fakeChannel) NotifyClose(ch chan *amqp.Error) chan *amqp.Error {
	f.closeNotify = ch
	return ch
}
func (f *fakeChannel) NotifyCancel(ch chan string) chan string { f.cancelNotify = ch; return ch }

func testConfig() Config {
	c := DefaultConfig
	c.Enabled = true
	c.Password = "s3cret"
	c.ReconnectInterval = time.Millisecond
	c.ReconnectMax = 4 * time.Millisecond
	return c
}

func newTestClient(t *testing.T, b *fakeBroker, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg, logp.NewLogger("test"))
	if err != nil {
		t.Fatal(err)
	}
	c.dial = b
	return c
}

func nextConn(t *testing.T, b *fakeBroker) *fakeConn {
	t.Helper()
	select {
	case c := <-b.connCh:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a consuming connection")
		return nil
	}
}

func recv(t *testing.T, out <-chan amqp.Delivery) amqp.Delivery {
	t.Helper()
	select {
	case d, ok := <-out:
		if !ok {
			t.Fatal("delivery channel closed")
		}
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a delivery")
		return amqp.Delivery{}
	}
}

func TestConsumeSetsUpBoundedQueue(t *testing.T) {
	b := newFakeBroker()
	c := newTestClient(t, b, testConfig())
	out := c.Consume(context.Background())
	defer c.Close()

	conn := nextConn(t, b)
	ch := conn.ch
	if !ch.exchangePassive || ch.exchangeActive {
		t.Errorf("exchange declare: passive=%v active=%v, want passive only", ch.exchangePassive, ch.exchangeActive)
	}
	if ch.queueArgs["x-message-ttl"] != int64(300000) || ch.queueArgs["x-max-length"] != int64(50000) || ch.queueArgs["x-overflow"] != "drop-head" {
		t.Errorf("queue args = %v", ch.queueArgs)
	}
	if ch.bindKey != "demod.metrics" || ch.prefetch != 500 {
		t.Errorf("bind key %q, prefetch %d", ch.bindKey, ch.prefetch)
	}

	ch.deliveries <- amqp.Delivery{Body: []byte("one")}
	if d := recv(t, out); string(d.Body) != "one" {
		t.Errorf("body = %q", d.Body)
	}
}

func TestActiveExchangeDeclare(t *testing.T) {
	b := newFakeBroker()
	cfg := testConfig()
	cfg.ExchangeDeclare = DeclareActive
	c := newTestClient(t, b, cfg)
	c.Consume(context.Background())
	defer c.Close()
	if ch := nextConn(t, b).ch; !ch.exchangeActive || ch.exchangePassive {
		t.Errorf("exchange declare: passive=%v active=%v, want active only", ch.exchangePassive, ch.exchangeActive)
	}
}

// Every way a session can end must lead to a fresh connection, while the
// caller's delivery channel stays open.
func TestReconnects(t *testing.T) {
	ends := map[string]func(*fakeConn){
		"connection closed": func(c *fakeConn) { c.notify <- amqp.ErrClosed },
		"channel closed":    func(c *fakeConn) { c.ch.closeNotify <- &amqp.Error{Code: 406, Reason: "PRECONDITION_FAILED"} },
		"consumer cancel":   func(c *fakeConn) { c.ch.cancelNotify <- consumerTag },
		"deliveries closed": func(c *fakeConn) { close(c.ch.deliveries) },
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			b := newFakeBroker()
			c := newTestClient(t, b, testConfig())
			out := c.Consume(context.Background())
			defer c.Close()

			first := nextConn(t, b)
			end(first)
			second := nextConn(t, b)
			if !first.isClosed() {
				t.Error("old connection not closed")
			}
			second.ch.deliveries <- amqp.Delivery{Body: []byte("after")}
			if d := recv(t, out); string(d.Body) != "after" {
				t.Errorf("body = %q", d.Body)
			}
			if got := c.Reconnects(); got != 1 {
				t.Errorf("Reconnects() = %d, want 1", got)
			}
		})
	}
}

func TestRetriesFailedDial(t *testing.T) {
	b := newFakeBroker()
	b.failDial = 3
	c := newTestClient(t, b, testConfig())
	c.Consume(context.Background())
	defer c.Close()
	nextConn(t, b)
	if got := c.Reconnects(); got != 3 {
		t.Errorf("Reconnects() = %d, want 3", got)
	}
}

func TestCancelClosesOutput(t *testing.T) {
	b := newFakeBroker()
	c := newTestClient(t, b, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	out := c.Consume(ctx)
	conn := nextConn(t, b)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("unexpected delivery")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("output not closed after cancel")
	}
	c.Close()
	c.Close() // idempotent
	if !conn.isClosed() {
		t.Error("connection not closed on shutdown")
	}
}

func TestCloseBeforeConsume(t *testing.T) {
	c := newTestClient(t, newFakeBroker(), testConfig())
	c.Close()
}

func TestBackoff(t *testing.T) {
	d := time.Second
	var got []time.Duration
	for range 6 {
		d = nextBackoff(d, 30*time.Second)
		got = append(got, d)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff sequence = %v, want %v", got, want)
		}
	}
	for range 100 {
		if j := jitter(10 * time.Second); j < 5*time.Second || j > 10*time.Second {
			t.Fatalf("jitter(10s) = %v, want 5s–10s", j)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig.Validate(); err != nil {
		t.Errorf("disabled default config: %v", err)
	}
	cfg := DefaultConfig
	cfg.Enabled = true
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "exchange") {
		t.Errorf("enabled default config: err = %v, want only the password required", err)
	}
	if DefaultConfig.Exchange != "x.tropobeat.demod" || DefaultConfig.RoutingKey != "demod.metrics" {
		t.Errorf("default exchange %q, routing key %q", DefaultConfig.Exchange, DefaultConfig.RoutingKey)
	}
	if err := testConfig().Validate(); err != nil {
		t.Errorf("complete config: %v", err)
	}
	bad := testConfig()
	bad.ExchangeDeclare = "maybe"
	if bad.Validate() == nil {
		t.Error("bad exchange_declare accepted")
	}
	bad = testConfig()
	bad.ReconnectMax = 0
	if bad.Validate() == nil {
		t.Error("reconnect_max below reconnect_interval accepted")
	}
}

func TestRedact(t *testing.T) {
	c := newTestClient(t, newFakeBroker(), testConfig())
	if s := c.redact(errors.New("dial amqp://insite:s3cret@host")); strings.Contains(s, "s3cret") {
		t.Errorf("password not redacted: %s", s)
	}
}
