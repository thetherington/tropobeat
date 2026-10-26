// Package correlator decides, as live demod reports arrive, whether a
// channel's degradation is co-channel interference (CCI) carried over a
// tropospheric duct.
//
// For every demod input it checks each report against the input's own
// 7-day baseline (BER spike, MER drop, lock loss) and applies a hysteresis,
// so an input is "degraded" only after several anomalous reports in a row.
// It then pairs the side A and side B inputs of each LRF and RF channel.
// The two sides share one antenna feed, so when both are degraded the
// problem is in the received signal, not one demod. Finally it looks up the
// ducting on the co-channel paths of that channel: a path with dM/dh < 0
// (ducting or trapping) is the environmental evidence that names CCI, and
// the interfering transmitter, as the root cause. CORRELATION.md describes
// the rules for operators.
//
// A single actor goroutine (started by New, stopped by Close) owns all
// mutable state, so nothing is locked:
//
//	run():
//	  ticker = clock.NewTicker(SweepInterval)
//	  for select:
//	    <-closed          -> return
//	    r := <-in         -> ingest(r): judge the report, update the input's
//	                         hysteresis, re-evaluate its (LRF, channel) pair
//	    now := <-ticker   -> sweep(now): expire silent inputs, re-evaluate
//	                         every pair (stale sides, grace periods, ducting
//	                         changes, ongoing snapshots)
//	    events <- next    -> hand the oldest pending event to the caller
//	    traces <- next    -> hand the oldest pending trace record to the caller
//
// Events and trace records wait in the actor's own outbox until the caller
// takes them, so a slow caller never blocks ingest and a caller that is
// itself blocked in Submit can't deadlock the engine. Events are never
// dropped; trace records beyond Trace.Buffer are (and are counted).
package correlator

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
)

// Report conditions (InstanceEvidence.Conditions) and input trace reasons.
const (
	CondBERSpike     = "ber_spike"
	CondBERSaturated = "ber_saturated"
	CondMERDrop      = "mer_drop"
	CondLockLost     = "lock_lost"
)

// forgetAfter is how long an expired input is remembered before it is
// removed, so a returning input can be traced as a gap restart.
const forgetAfter = time.Hour

// skipTraceEvery limits skip records to one per input per period.
const skipTraceEvery = time.Hour

// pairTraceEvery is how often an unchanged non-clear pair is traced.
const pairTraceEvery = time.Minute

// powerRing is how many recent clean power readings an input keeps, to
// compare with its power once degraded.
const powerRing = 10

// Option configures an Engine at construction time.
type Option func(*Engine)

// WithBaselines injects the baseline source. Without one, every report is
// skipped as having no baseline.
func WithBaselines(b Baselines) Option { return func(e *Engine) { e.baselines = b } }

// WithDucting injects the ducting source. Without one, ducting is never
// known and no pair can be confirmed.
func WithDucting(d Ducting) Option { return func(e *Engine) { e.ducting = d } }

// WithClock injects the engine's time source. Defaults to RealClock{}.
func WithClock(c Clock) Option { return func(e *Engine) { e.clock = c } }

// Stats is a snapshot of the engine's counters, safe to read from any
// goroutine.
type Stats struct {
	Submitted int64 // reports received
	Evaluated int64 // reports judged against a usable baseline
	Skipped   int64 // reports with no usable baseline
	Ingests   int64 // completed ingests; tests use it to wait for a report
	Sweeps    int64 // completed sweeps
	Events    int64 // events handed to the caller
	Traces    int64 // trace records handed to the caller
	// TraceDropped counts trace records dropped because Trace.Buffer
	// records were already waiting.
	TraceDropped int64
	// Instances is the number of inputs with live state.
	Instances int64
	// Pairs is the number of pairs in each state.
	Pairs map[State]int64
}

type counters struct {
	submitted, evaluated, skipped, ingests, sweeps atomic.Int64
	events, traces, traceDropped, instances        atomic.Int64
	pairs                                          [6]atomic.Int64
}

// Engine is the correlation engine. Construct with New; it owns a goroutine
// until Close is called.
type Engine struct {
	cfg       Config
	cats      categorySet
	traceLRFs map[string]bool
	baselines Baselines
	ducting   Ducting
	clock     Clock

	in        chan demod.Report
	events    chan Event
	traces    chan Trace
	closed    chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	// Actor-owned state.
	instances map[demod.Instance]*instanceState
	pairs     map[demod.ChannelKey]*pairState
	// frames records the latest lock status of every input on each demod
	// frame (host IP), including inputs without a baseline, for the
	// lock-loss guard.
	frames    map[string]map[demod.Instance]frameSeen
	skipSeen  map[demod.Instance]time.Time
	outEvents []Event
	outTraces []Trace

	c counters
}

type frameSeen struct {
	at     time.Time
	locked bool
}

// conditions are the anomalies on one report.
type conditions struct {
	berSpike, saturated, merDrop, lockLost bool
}

func (c conditions) anomalous(lockLoss bool) bool {
	return c.berSpike || c.saturated || c.merDrop || (lockLoss && c.lockLost)
}

func (c conditions) ber() bool { return c.berSpike || c.saturated }

// lockOnly reports whether lock loss is the only anomaly.
func (c conditions) lockOnly() bool { return c.lockLost && !c.ber() && !c.merDrop }

func (c conditions) list() []string {
	var out []string
	if c.berSpike {
		out = append(out, CondBERSpike)
	}
	if c.saturated {
		out = append(out, CondBERSaturated)
	}
	if c.merDrop {
		out = append(out, CondMERDrop)
	}
	if c.lockLost {
		out = append(out, CondLockLost)
	}
	return out
}

// instanceState is the live state of one demod input.
type instanceState struct {
	inst  demod.Instance
	key   demod.ChannelKey
	last  demod.Report
	seen  time.Time
	base  Baseline
	cond  conditions
	alive bool // false once expired by the sweep

	// bad and good count consecutive anomalous and clean reports.
	bad, good int
	degraded  bool
	since     time.Time
	// berBad and berGood do the same for the BER-only Stressed flag.
	berBad, berGood int
	stressed        bool

	powers   []int
	prePower float64
	hasPre   bool
}

// pairState is the verdict state of one LRF and RF channel.
type pairState struct {
	key     demod.ChannelKey
	members map[demod.Instance]struct{}

	state       State
	reason      string
	strength    ducting.State
	strongestID string

	episodeID    string
	episodeStart time.Time
	peak         State
	lastEvent    time.Time

	// cand is a one-sided verdict waiting for PairGrace since candSince.
	cand      State
	candSince time.Time

	// counted is whether each side (A, B) counted as degraded in the last
	// classification. A recovering side keeps counting only if it did.
	counted [2]bool

	lastTrace       time.Time
	lastTraceState  State
	lastTraceReason string
}

// New starts an engine. It returns an error if cfg is invalid.
func New(cfg Config, opts ...Option) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cats, err := parseCategories(cfg.Trace.Categories)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:       cfg,
		cats:      cats,
		clock:     RealClock{},
		in:        make(chan demod.Report, cfg.InputBuffer),
		events:    make(chan Event),
		traces:    make(chan Trace),
		closed:    make(chan struct{}),
		done:      make(chan struct{}),
		instances: map[demod.Instance]*instanceState{},
		pairs:     map[demod.ChannelKey]*pairState{},
		frames:    map[string]map[demod.Instance]frameSeen{},
		skipSeen:  map[demod.Instance]time.Time{},
	}
	if len(cfg.Trace.LRFSites) > 0 {
		e.traceLRFs = map[string]bool{}
		for _, s := range cfg.Trace.LRFSites {
			e.traceLRFs[demod.NormalizeLRF(s)] = true
		}
	}
	for _, o := range opts {
		o(e)
	}
	go e.run()
	return e, nil
}

// Submit hands a report to the engine, waiting while its queue is full. It
// returns false once the engine is closed.
func (e *Engine) Submit(r demod.Report) bool {
	select {
	case <-e.closed:
		return false
	default:
	}
	select {
	case <-e.closed:
		return false
	case e.in <- r:
		return true
	}
}

// TrySubmit is Submit without waiting: it returns false when the queue is
// full or the engine is closed.
func (e *Engine) TrySubmit(r demod.Report) bool {
	select {
	case <-e.closed:
		return false
	default:
	}
	select {
	case e.in <- r:
		return true
	default:
		return false
	}
}

// Events returns the channel of verdict events. It is closed by Close.
func (e *Engine) Events() <-chan Event { return e.events }

// Traces returns the channel of decision-trace records. It is closed by
// Close. Nothing is sent when no trace category is enabled.
func (e *Engine) Traces() <-chan Trace { return e.traces }

// Tracing reports whether any trace category is enabled.
func (e *Engine) Tracing() bool { return e.cats != 0 }

// Close stops the engine and waits for its goroutine to finish. Pending
// events and trace records are discarded. It is safe to call more than once.
func (e *Engine) Close() {
	e.closeOnce.Do(func() { close(e.closed) })
	<-e.done
}

// Stats returns a snapshot of the engine's counters.
func (e *Engine) Stats() Stats {
	s := Stats{
		Submitted:    e.c.submitted.Load(),
		Evaluated:    e.c.evaluated.Load(),
		Skipped:      e.c.skipped.Load(),
		Ingests:      e.c.ingests.Load(),
		Sweeps:       e.c.sweeps.Load(),
		Events:       e.c.events.Load(),
		Traces:       e.c.traces.Load(),
		TraceDropped: e.c.traceDropped.Load(),
		Instances:    e.c.instances.Load(),
		Pairs:        map[State]int64{},
	}
	for i, st := range States {
		s.Pairs[st] = e.c.pairs[i].Load()
	}
	return s
}

func (e *Engine) run() {
	defer close(e.done)
	defer close(e.events)
	defer close(e.traces)
	ticker := e.clock.NewTicker(e.cfg.SweepInterval)
	defer ticker.Stop()

	for {
		// A nil channel never sends, so the outbox cases only fire when
		// something is waiting.
		var (
			evOut  chan<- Event
			evNext Event
			trOut  chan<- Trace
			trNext Trace
		)
		if len(e.outEvents) > 0 {
			evOut, evNext = e.events, e.outEvents[0]
		}
		if len(e.outTraces) > 0 {
			trOut, trNext = e.traces, e.outTraces[0]
		}
		select {
		case <-e.closed:
			return
		case r := <-e.in:
			e.ingest(r)
			e.c.ingests.Add(1)
		case now := <-ticker.C():
			e.sweep(now)
			e.c.sweeps.Add(1)
		case evOut <- evNext:
			e.outEvents[0] = Event{}
			e.outEvents = e.outEvents[1:]
			e.c.events.Add(1)
		case trOut <- trNext:
			e.outTraces[0] = Trace{}
			e.outTraces = e.outTraces[1:]
			e.c.traces.Add(1)
		}
	}
}

// ingest judges one report and re-evaluates its pair.
func (e *Engine) ingest(r demod.Report) {
	now := e.clock.Now()
	e.c.submitted.Add(1)
	inst := r.Instance()

	frame := e.frames[inst.HostIP]
	if frame == nil {
		frame = map[demod.Instance]frameSeen{}
		e.frames[inst.HostIP] = frame
	}
	frame[inst] = frameSeen{at: now, locked: r.Locked()}

	b, ok := Baseline{}, false
	if e.baselines != nil {
		b, ok = e.baselines.Lookup(inst, r.RFChannel)
	}
	if !ok || !b.Usable() {
		e.c.skipped.Add(1)
		reason := "no_baseline"
		if ok {
			reason = "baseline_" + b.Status
		}
		e.traceSkip(r, reason, fmt.Sprintf("no usable baseline for %s on RF %d (%s); the report is not judged", inst, r.RFChannel, strings.ReplaceAll(reason, "_", " ")), now)
		// An input that loses its baseline (a new baseline run) stops
		// counting towards its pair.
		if st := e.instances[inst]; st != nil {
			e.removeInstance(st, now)
		}
		return
	}
	e.c.evaluated.Add(1)
	if r.Side != "A" && r.Side != "B" {
		e.traceSkip(r, "no_side", fmt.Sprintf("%s reports side %q, so it is judged but left out of A/B pairing", inst, r.Side), now)
	}

	st := e.instances[inst]
	if st != nil && st.key != r.Channel() {
		// Retuned to another channel: start again on the new one.
		e.removeInstance(st, now)
		st = nil
	}
	if st == nil {
		st = &instanceState{inst: inst, key: r.Channel(), alive: true}
		e.instances[inst] = st
		e.c.instances.Add(1)
		p := e.pairs[st.key]
		if p == nil {
			p = &pairState{key: st.key, members: map[demod.Instance]struct{}{}, state: StateClear}
			e.pairs[st.key] = p
			e.c.pairs[0].Add(1)
		}
		p.members[inst] = struct{}{}
	} else if !st.alive || now.Sub(st.seen) > e.cfg.ExpireAfter {
		gap := now.Sub(st.seen).Round(time.Second)
		st.resetCounters()
		st.alive = true
		e.traceInput(CategoryHysteresis, st, "gap_reset", DecisionMiss,
			fmt.Sprintf("no report for %s (over expire_after %s); counters restart", gap, e.cfg.ExpireAfter), now)
	}
	st.last, st.seen, st.base = r, now, b
	st.cond = e.judge(r, b)

	e.updateHysteresis(st, now)
	// After the hysteresis update, so the record shows the counts this
	// report produced.
	if e.tracing(CategoryInput, st.key.LRF) {
		reason, decision := "ok", DecisionMiss
		if l := st.cond.list(); len(l) > 0 {
			reason = l[0]
			if st.cond.anomalous(e.cfg.LockLoss) {
				decision = DecisionHit
			}
		}
		e.traceInput(CategoryInput, st, reason, decision, e.inputMessage(st), now)
	}
	e.evaluatePair(e.pairs[st.key], now, false)
}

// judge returns the anomalies of a report against its baseline.
func (e *Engine) judge(r demod.Report, b Baseline) conditions {
	if !r.Locked() {
		return conditions{lockLost: true}
	}
	var c conditions
	if r.Saturated() {
		c.saturated = true
	} else {
		c.berSpike = r.BER >= e.cfg.BERFactor*b.EffectiveBER
	}
	c.merDrop = float64(r.MER) <= b.MedianMER-e.cfg.MERDropDB
	return c
}

func (st *instanceState) resetCounters() {
	st.bad, st.good, st.berBad, st.berGood = 0, 0, 0, 0
	st.degraded, st.stressed, st.hasPre = false, false, false
	st.since = time.Time{}
}

// updateHysteresis counts the report towards the input's degraded and
// Stressed states.
func (e *Engine) updateHysteresis(st *instanceState, now time.Time) {
	n := e.cfg.HysteresisCycles
	if st.cond.anomalous(e.cfg.LockLoss) {
		st.bad, st.good = st.bad+1, 0
	} else {
		st.bad, st.good = 0, st.good+1
	}
	switch {
	case !st.degraded && st.bad >= n:
		st.degraded, st.since = true, now
		st.prePower, st.hasPre = medianInt(st.powers)
		e.traceInput(CategoryHysteresis, st, "degraded_enter", DecisionHit,
			fmt.Sprintf("%d anomalous reports in a row (%s): input degraded", st.bad, strings.Join(st.cond.list(), ", ")), now)
	case st.degraded && st.good >= n:
		st.degraded, st.hasPre = false, false
		e.traceInput(CategoryHysteresis, st, "degraded_clear", DecisionMiss,
			fmt.Sprintf("%d clean reports in a row: input no longer degraded (was since %s)", st.good, st.since.UTC().Format(time.RFC3339)), now)
		st.since = time.Time{}
	}
	if !st.degraded && st.last.Locked() && !st.cond.anomalous(e.cfg.LockLoss) {
		st.powers = append(st.powers, st.last.PowerDBm)
		if len(st.powers) > powerRing {
			st.powers = st.powers[1:]
		}
	}

	if st.cond.ber() {
		st.berBad, st.berGood = st.berBad+1, 0
	} else {
		st.berBad, st.berGood = 0, st.berGood+1
	}
	switch {
	case !st.stressed && st.berBad >= n:
		st.stressed = true
		e.traceInput(CategoryHysteresis, st, "stressed_enter", DecisionHit,
			fmt.Sprintf("BER spike held for %d reports while locked: Environmentally Stressed", st.berBad), now)
	case st.stressed && st.berGood >= n:
		st.stressed = false
		e.traceInput(CategoryHysteresis, st, "stressed_clear", DecisionMiss,
			fmt.Sprintf("%d reports without a BER spike: no longer Stressed", st.berGood), now)
	}
}

// removeInstance forgets an input and re-evaluates its old pair.
func (e *Engine) removeInstance(st *instanceState, now time.Time) {
	delete(e.instances, st.inst)
	e.c.instances.Add(-1)
	if p := e.pairs[st.key]; p != nil {
		delete(p.members, st.inst)
		e.evaluatePair(p, now, false)
	}
}

// sweep expires silent inputs and re-evaluates every pair.
func (e *Engine) sweep(now time.Time) {
	sum := SweepSummary{}
	for _, st := range e.instances {
		silent := now.Sub(st.seen)
		if st.alive && silent > e.cfg.ExpireAfter {
			st.alive = false
			st.resetCounters()
			sum.Expired++
			e.traceInput(CategoryHysteresis, st, "expired", DecisionMiss,
				fmt.Sprintf("no report for %s (over expire_after %s): live state dropped", silent.Round(time.Second), e.cfg.ExpireAfter), now)
		}
		if !st.alive && silent > forgetAfter {
			e.removeInstance(st, now)
		}
	}
	for ip, frame := range e.frames {
		for inst, fs := range frame {
			if now.Sub(fs.at) > e.cfg.ExpireAfter {
				delete(frame, inst)
			}
		}
		if len(frame) == 0 {
			delete(e.frames, ip)
		}
	}
	for inst, at := range e.skipSeen {
		if now.Sub(at) > skipTraceEvery {
			delete(e.skipSeen, inst)
		}
	}
	for key, p := range e.pairs {
		if len(p.members) == 0 && p.state == StateClear {
			delete(e.pairs, key)
			e.c.pairs[0].Add(-1)
			continue
		}
		e.evaluatePair(p, now, true)
		sum.Reevaluated++
		if p.state != StateClear {
			sum.NotClear++
		}
	}
	if e.cats.has(CategorySweep) {
		sum.Instances, sum.Pairs = len(e.instances), len(e.pairs)
		sum.TraceDropped = e.c.traceDropped.Load()
		e.emitTrace(Trace{
			Category: CategorySweep, At: now, Reason: "summary", Decision: DecisionSkip,
			Message: fmt.Sprintf("%d inputs, %d expired, %d pairs (%d not clear), %d trace records dropped so far",
				sum.Instances, sum.Expired, sum.Pairs, sum.NotClear, sum.TraceDropped),
			Sweep: &sum,
		})
	}
}

// sideView judges one side of a pair from its fresh inputs.
func (e *Engine) sideView(p *pairState, side string, now time.Time) SideEvidence {
	ev := SideEvidence{Side: side, View: ViewMissing}
	var any, fresh, degraded, guarded bool
	// recovering stays true while every degraded input's latest report is
	// clean.
	recovering := true
	for _, inst := range sortedInstances(p.members) {
		st := e.instances[inst]
		if st == nil || st.last.Side != side {
			continue
		}
		any = true
		ie := e.evidence(st, now)
		ev.Instances = append(ev.Instances, ie)
		if !ie.Fresh {
			continue
		}
		fresh = true
		if !st.degraded {
			continue
		}
		if st.cond.lockOnly() && !e.frameHasLocked(st, now) {
			guarded = true
			continue
		}
		degraded = true
		if st.cond.anomalous(e.cfg.LockLoss) {
			recovering = false
		}
		if ev.Since.IsZero() || st.since.Before(ev.Since) {
			ev.Since = st.since
		}
	}
	switch {
	case degraded && recovering:
		ev.View = ViewRecovering
	case degraded:
		ev.View = ViewDegraded
	case guarded:
		ev.View = ViewGuarded
	case fresh:
		ev.View = ViewClean
	case any:
		ev.View = ViewStale
	}
	return ev
}

// frameHasLocked reports whether another input on the same frame has a
// recent locked report, i.e. the frame itself is up.
func (e *Engine) frameHasLocked(st *instanceState, now time.Time) bool {
	for inst, fs := range e.frames[st.inst.HostIP] {
		if inst != st.inst && fs.locked && now.Sub(fs.at) <= e.cfg.StaleAfter {
			return true
		}
	}
	return false
}

// verdict is the outcome of classifying a pair.
type verdict struct {
	state    State
	reason   string
	sides    []SideEvidence
	ducting  ChannelDucting
	evidence DuctingEvidence
	strong   PathEvidence
	hasPath  bool
	counted  [2]bool
	// sustained: every side that counts is only recovering, so the verdict
	// may stay or clear but not change to another.
	sustained bool
}

// classify works out the pair's state from its two sides and the ducting.
func (e *Engine) classify(p *pairState, now time.Time) verdict {
	a, b := e.sideView(p, "A", now), e.sideView(p, "B", now)
	v := verdict{sides: []SideEvidence{a, b}}
	if e.ducting != nil {
		v.ducting = e.ducting.Channel(p.key.LRF, p.key.RFChannel, now)
	} else {
		v.ducting = ChannelDucting{Status: DuctingNoPoll}
	}
	v.strong, v.hasPath = v.ducting.Strongest()
	v.evidence = ductingEvidence(v.ducting)

	// A recovering side only keeps going an open verdict it counted towards,
	// so a frame coming back from an outage (guarded) never joins one.
	counts := func(i int, s SideEvidence) bool {
		return s.View == ViewDegraded || (s.View == ViewRecovering && p.state != StateClear && p.counted[i])
	}
	dA, dB := counts(0, a), counts(1, b)
	v.counted = [2]bool{dA, dB}
	v.sustained = (dA || dB) && !(dA && a.View == ViewDegraded) && !(dB && b.View == ViewDegraded)
	switch {
	case !dA && !dB:
		v.state, v.reason = StateClear, "clear"
		if a.View == ViewGuarded || b.View == ViewGuarded {
			v.reason = "guard_frame_down"
		}
	case dA && dB:
		switch {
		case !v.ducting.Known():
			v.state, v.reason = StateDualDuctingUnknown, "ducting_"+v.ducting.Status
		case !v.hasPath:
			v.state, v.reason = StateDualUnexplained, "no_cochannel_paths"
		case v.strong.Known() && v.strong.State.Level() >= ducting.StateDucting.Level():
			v.state, v.reason = StateCCIConfirmed, "cci_confirmed"
		case v.ducting.Unknown() > 0:
			// No known path is ducting, but some have no weather: one of
			// them could be.
			v.state, v.reason = StateDualDuctingUnknown, "ducting_incomplete"
		default:
			v.state, v.reason = StateDualUnexplained, "ducting_none_on_channel"
		}
	default:
		other := b
		if dB {
			other = a
		}
		switch other.View {
		case ViewClean, ViewRecovering:
			v.state, v.reason = StateSingleSide, "single_side"
		case ViewGuarded:
			v.state, v.reason = StateUnpaired, "guard_frame_down"
		case ViewStale:
			v.state, v.reason = StateUnpaired, "other_side_stale"
		default:
			v.state, v.reason = StateUnpaired, "other_side_missing"
		}
	}
	return v
}

func ductingEvidence(cd ChannelDucting) DuctingEvidence {
	d := DuctingEvidence{Status: cd.Status, PolledAt: cd.PolledAt, ForecastTime: cd.ForecastTime, PathsOnChannel: len(cd.Paths), PathsUnknown: cd.Unknown()}
	if s, ok := cd.Strongest(); ok {
		d.Strongest = &s
	}
	for _, p := range cd.Paths {
		if p.State.Level() >= ducting.StateDucting.Level() {
			d.Active = append(d.Active, p)
		}
	}
	return d
}

// evaluatePair classifies a pair, applies the one-sided grace period and
// emits an event when the verdict starts, changes, clears, gains new
// ducting evidence, or (on a sweep) is due an ongoing snapshot.
func (e *Engine) evaluatePair(p *pairState, now time.Time, fromSweep bool) {
	if p == nil {
		return
	}
	v := e.classify(p, now)
	p.counted = v.counted

	target, held := v.state, false
	if v.sustained && v.state != p.state {
		// Only recovering sides count: keep the open verdict until it clears.
		target, held = p.state, true
	} else if v.state != p.state && v.state.oneSided() && e.cfg.PairGrace > 0 {
		if p.cand != v.state {
			p.cand, p.candSince = v.state, now
		}
		if now.Sub(p.candSince) < e.cfg.PairGrace {
			target, held = p.state, true
		}
	}
	if !held {
		p.cand = ""
	}

	prev := p.state
	emitted := false
	switch {
	case target != prev:
		e.c.pairs[prev.Severity()].Add(-1)
		e.c.pairs[target.Severity()].Add(1)
		kind := KindUpdate
		switch {
		case prev == StateClear:
			kind = KindStart
			p.episodeStart = now
			p.episodeID = fmt.Sprintf("%s:%d:%s", p.key.LRF, p.key.RFChannel, now.UTC().Format("20060102T150405Z"))
			p.peak = target
		case target == StateClear:
			kind = KindClear
		}
		if target.Severity() > p.peak.Severity() {
			p.peak = target
		}
		p.state, p.reason = target, v.reason
		p.setStrength(v)
		e.emitEvent(p, kind, prev, v, now)
		emitted = true
		if target == StateClear {
			p.episodeID, p.episodeStart, p.peak = "", time.Time{}, ""
		}
	case held:
		// Keep the current state and its reason until the grace ends.
	case target == StateCCIConfirmed && (v.strong.State != p.strength || v.strong.PathID != p.strongestID):
		p.reason = v.reason
		p.setStrength(v)
		e.emitEvent(p, KindUpdate, prev, v, now)
		emitted = true
	default:
		p.reason = v.reason
	}
	if fromSweep && !emitted && p.state != StateClear && e.cfg.SnapshotInterval > 0 &&
		now.Sub(p.lastEvent) >= e.cfg.SnapshotInterval {
		e.emitEvent(p, KindOngoing, p.state, v, now)
	}
	e.tracePair(p, v, target, held, now)
}

func (p *pairState) setStrength(v verdict) {
	if p.state == StateCCIConfirmed {
		p.strength, p.strongestID = v.strong.State, v.strong.PathID
	} else {
		p.strength, p.strongestID = "", ""
	}
}

func (e *Engine) emitEvent(p *pairState, kind EventKind, prev State, v verdict, now time.Time) {
	ev := Event{
		Kind: kind, At: now, LRF: p.key.LRF, RFChannel: p.key.RFChannel,
		State: p.state, Previous: prev, Reason: p.reason,
		EpisodeID: p.episodeID, EpisodeStart: p.episodeStart, Peak: p.peak,
		Sides: v.sides, Ducting: v.evidence,
	}
	if kind == KindClear {
		ev.Reason = "clear"
	}
	if p.state == StateCCIConfirmed {
		ev.Strength = p.strength
	}
	if !p.episodeStart.IsZero() && kind != KindStart {
		ev.Duration = now.Sub(p.episodeStart)
	}
	p.lastEvent = now
	e.outEvents = append(e.outEvents, ev)
}

// tracing reports whether records of category c are wanted for an LRF.
func (e *Engine) tracing(c Category, lrf string) bool {
	return e.cats.has(c) && (e.traceLRFs == nil || e.traceLRFs[lrf])
}

func (e *Engine) emitTrace(t Trace) {
	if len(e.outTraces) >= e.cfg.Trace.Buffer {
		e.c.traceDropped.Add(1)
		return
	}
	e.outTraces = append(e.outTraces, t)
}

func (e *Engine) traceInput(c Category, st *instanceState, reason, decision, msg string, now time.Time) {
	if !e.tracing(c, st.key.LRF) {
		return
	}
	ie := e.evidence(st, now)
	e.emitTrace(Trace{
		Category: c, At: now, Reason: reason, Decision: decision, Message: msg,
		LRF: st.key.LRF, RFChannel: st.key.RFChannel, EpisodeID: e.episodeOf(st.key),
		Input: &ie,
	})
}

func (e *Engine) traceSkip(r demod.Report, reason, msg string, now time.Time) {
	if !e.tracing(CategorySkip, r.LRF) {
		return
	}
	inst := r.Instance()
	if at, ok := e.skipSeen[inst]; ok && now.Sub(at) < skipTraceEvery {
		return
	}
	e.skipSeen[inst] = now
	e.emitTrace(Trace{
		Category: CategorySkip, At: now, Reason: reason, Decision: DecisionSkip, Message: msg,
		LRF: r.LRF, RFChannel: r.RFChannel,
		Input: &InstanceEvidence{
			Instance: inst, Side: r.Side, CardLabel: r.CardLabel, FrameLabel: r.FrameLabel,
			ReportTime: r.Timestamp, Fresh: true, Locked: r.Locked(), BER: r.BER, MER: r.MER, PowerDBm: r.PowerDBm,
		},
	})
}

// tracePair records a pair decision: when its state or reason changes, and
// once a minute while it is not clear (or a one-sided verdict is waiting).
func (e *Engine) tracePair(p *pairState, v verdict, target State, held bool, now time.Time) {
	lrf := p.key.LRF
	if !e.tracing(CategoryPair, lrf) && !e.tracing(CategoryDucting, lrf) {
		return
	}
	quiet := v.state == StateClear && v.reason == "clear" && (p.lastTraceState == "" || p.lastTraceState == StateClear)
	changed := v.state != p.lastTraceState || v.reason != p.lastTraceReason
	due := (target != StateClear || held) && now.Sub(p.lastTrace) >= pairTraceEvery
	if quiet || !(changed || due) {
		if quiet {
			p.lastTraceState, p.lastTraceReason = v.state, v.reason
		}
		return
	}
	p.lastTrace, p.lastTraceState, p.lastTraceReason = now, v.state, v.reason

	if e.tracing(CategoryPair, lrf) {
		decision := DecisionMiss
		switch {
		case held:
			decision = DecisionSkip
		case v.state == StateCCIConfirmed:
			decision = DecisionHit
		}
		de := v.evidence
		e.emitTrace(Trace{
			Category: CategoryPair, At: now, Reason: v.reason, Decision: decision,
			Message: e.pairMessage(p, v, held, now),
			LRF:     lrf, RFChannel: p.key.RFChannel, EpisodeID: p.episodeID,
			State: target, Sides: v.sides, Ducting: &de,
		})
	}
	if e.tracing(CategoryDucting, lrf) && (v.state.dual() || v.state == StateCCIConfirmed) {
		reason, msg := ductingReason(v)
		decision := DecisionMiss
		if v.ducting.Known() && v.hasPath && v.strong.State.Level() >= ducting.StateDucting.Level() {
			decision = DecisionHit
		}
		de := v.evidence
		e.emitTrace(Trace{
			Category: CategoryDucting, At: now, Reason: reason, Decision: decision, Message: msg,
			LRF: lrf, RFChannel: p.key.RFChannel, EpisodeID: p.episodeID, Ducting: &de,
		})
	}
}

func ductingReason(v verdict) (reason, msg string) {
	cd := v.ducting
	switch {
	case cd.Status == DuctingNoPoll:
		return "no_poll", "no weather poll has finished yet, so ducting is unknown"
	case cd.Status == DuctingStale:
		return "poll_stale", fmt.Sprintf("the last weather poll (%s) is too old to use", cd.PolledAt.UTC().Format(time.RFC3339))
	case !v.hasPath:
		return "no_cochannel_paths", "the channel has no co-channel paths, so ducting can't explain it"
	}
	active := 0
	for _, p := range cd.Paths {
		if p.State.Level() >= ducting.StateDucting.Level() {
			active++
		}
	}
	s := v.strong
	switch {
	case s.State == ducting.StateTrapping:
		reason = "path_trapping"
	case s.State == ducting.StateDucting:
		reason = "path_ducting"
	case cd.Unknown() > 0:
		reason = "path_no_data"
	default:
		reason = "path_below_ducting"
	}
	strongest := fmt.Sprintf("strongest %s (%s, %.0f mi) is %s", s.PathID, s.Name(), s.DistanceMi, s.State)
	if !s.Known() {
		strongest = "no path has usable weather"
	}
	return reason, fmt.Sprintf("%d of %d co-channel paths ducting or trapping, %d with no usable weather; %s",
		active, len(cd.Paths), cd.Unknown(), strongest)
}

func (e *Engine) pairMessage(p *pairState, v verdict, held bool, now time.Time) string {
	a, b := v.sides[0], v.sides[1]
	sides := fmt.Sprintf("A %s, B %s", a.View, b.View)
	var msg string
	switch v.reason {
	case "clear":
		msg = "no side degraded"
	case "guard_frame_down":
		msg = "a side is degraded only by lock loss while its whole frame is unlocked; treated as a frame or site problem, not CCI"
	case "single_side":
		msg = "only one side is degraded and the other is clean: a demod or equipment fault, not the received signal"
	case "other_side_stale":
		msg = fmt.Sprintf("the other side has no report in the last %s, so the pair can't be confirmed", e.cfg.StaleAfter)
	case "other_side_missing":
		msg = "no input on the other side is known for this channel, so the pair can't be confirmed"
	case "ducting_no_poll", "ducting_stale":
		msg = "both sides degraded, but there are no recent ducting results"
	case "ducting_incomplete":
		msg = fmt.Sprintf("both sides degraded and no known co-channel path is ducting, but %d of %d paths have no usable weather", v.ducting.Unknown(), len(v.ducting.Paths))
	case "no_cochannel_paths":
		msg = "both sides degraded, but the channel has no co-channel paths"
	case "ducting_none_on_channel":
		msg = fmt.Sprintf("both sides degraded, but none of the %d co-channel paths is ducting (strongest %s is %s)", len(v.ducting.Paths), v.strong.PathID, v.strong.State)
	case "cci_confirmed":
		msg = fmt.Sprintf("both sides degraded and %s (%s, %.0f mi) is %s: CCI confirmed", v.strong.PathID, v.strong.Name(), v.strong.DistanceMi, v.strong.State)
	}
	switch {
	case held && v.sustained:
		msg = fmt.Sprintf("%s; only recovering sides count, so %s stays until they clear", msg, p.state)
	case held:
		msg = fmt.Sprintf("%s; %s waits pair_grace (%s of %s), state stays %s",
			msg, v.state, now.Sub(p.candSince).Round(time.Second), e.cfg.PairGrace, p.state)
	}
	return sides + ": " + msg
}

func (e *Engine) inputMessage(st *instanceState) string {
	r, b := st.last, st.base
	if !r.Locked() {
		return fmt.Sprintf("not locked (status %q); bad %d, good %d", r.Status, st.bad, st.good)
	}
	ber := fmt.Sprintf("BER %g vs threshold %g", r.BER, e.cfg.BERFactor*b.EffectiveBER)
	if r.Saturated() {
		ber = "BER saturated"
	}
	return fmt.Sprintf("%s; MER %d dB vs threshold %g dB; bad %d, good %d", ber, r.MER, b.MedianMER-e.cfg.MERDropDB, st.bad, st.good)
}

// evidence describes an input's latest reading and judgement.
func (e *Engine) evidence(st *instanceState, now time.Time) InstanceEvidence {
	r := st.last
	ie := InstanceEvidence{
		Instance: st.inst, Side: r.Side, CardLabel: r.CardLabel, FrameLabel: r.FrameLabel,
		ReportTime: r.Timestamp, Fresh: st.alive && now.Sub(st.seen) <= e.cfg.StaleAfter,
		Locked: r.Locked(), BER: r.BER, MER: r.MER, PowerDBm: r.PowerDBm,
		EffectiveBER: st.base.EffectiveBER, BERThreshold: e.cfg.BERFactor * st.base.EffectiveBER,
		MedianMER: st.base.MedianMER, MERThreshold: st.base.MedianMER - e.cfg.MERDropDB,
		Conditions: st.cond.list(), Degraded: st.degraded, Stressed: st.stressed,
		Bad: st.bad, Good: st.good,
	}
	if st.degraded {
		switch {
		case !r.Locked():
			ie.Signature = SignatureUnlocked
		case st.hasPre:
			ie.PowerDeltaDB = float64(r.PowerDBm) - st.prePower
			ie.Signature = SignatureSteady
			if ie.PowerDeltaDB <= -e.cfg.PowerDropDB {
				ie.Signature = SignatureDropping
			}
		}
	}
	return ie
}

func (e *Engine) episodeOf(k demod.ChannelKey) string {
	if p := e.pairs[k]; p != nil {
		return p.episodeID
	}
	return ""
}

func sortedInstances(m map[demod.Instance]struct{}) []demod.Instance {
	out := make([]demod.Instance, 0, len(m))
	for i := range m {
		out = append(out, i)
	}
	slices.SortFunc(out, func(a, b demod.Instance) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// medianInt returns the median of vs; ok is false when vs is empty.
func medianInt(vs []int) (float64, bool) {
	if len(vs) == 0 {
		return 0, false
	}
	s := slices.Clone(vs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return float64(s[len(s)/2]), true
	}
	return float64(s[len(s)/2-1]+s[len(s)/2]) / 2, true
}
