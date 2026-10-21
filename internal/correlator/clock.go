package correlator

import "time"

// Ticker is the subset of *time.Ticker the engine needs, abstracted so tests
// can fire ticks by hand instead of waiting on a real timer.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock is the engine's injected time source. Every time-based decision
// (the sweep_interval ticker, freshness, hysteresis gaps and expiry) goes through
// this instead of calling time.Now()/time.NewTicker() directly, so tests can
// advance time deterministically.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// RealClock is the production Clock: it delegates straight to package time.
// It is the default when WithClock is not given.
type RealClock struct{}

// Now returns time.Now().
func (RealClock) Now() time.Time { return time.Now() }

// NewTicker wraps time.NewTicker.
func (RealClock) NewTicker(d time.Duration) Ticker {
	return realTicker{time.NewTicker(d)}
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
