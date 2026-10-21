package correlator

import (
	"sync"
	"testing"
	"time"

	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
)

// fakeClock is a manually advanced Clock whose ticker fires on tick().
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	ch  chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), ch: make(chan time.Time)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) NewTicker(time.Duration) Ticker { return fakeTicker{c.ch} }

type fakeTicker struct{ ch chan time.Time }

func (t fakeTicker) C() <-chan time.Time { return t.ch }
func (t fakeTicker) Stop()               {}

// fakeBaselines gives every input the same baseline unless overridden.
type fakeBaselines struct {
	def     Baseline
	missing map[demod.Instance]bool
}

func (f fakeBaselines) Lookup(inst demod.Instance, _ int) (Baseline, bool) {
	if f.missing[inst] {
		return Baseline{}, false
	}
	return f.def, true
}

// fakeDucting returns one ChannelDucting for every channel; set it with set.
type fakeDucting struct {
	mu sync.Mutex
	cd ChannelDucting
}

func (f *fakeDucting) Channel(string, int, time.Time) ChannelDucting {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cd
}

func (f *fakeDucting) set(cd ChannelDucting) {
	f.mu.Lock()
	f.cd = cd
	f.mu.Unlock()
}

func pathAt(id string, s ducting.State, mi float64) PathEvidence {
	return PathEvidence{PathID: id, CallSign: "W" + id, FacilityID: id, DistanceMi: mi, State: s}
}

func fresh(paths ...PathEvidence) ChannelDucting {
	return ChannelDucting{Status: DuctingFresh, Paths: paths}
}

// harness drives one engine with a fake clock.
type harness struct {
	t     *testing.T
	e     *Engine
	clk   *fakeClock
	duct  *fakeDucting
	n     int64 // reports submitted
	s     int64 // sweeps fired
	evs   []Event
	trace []Trace
}

var (
	instA  = demod.Instance{HostIP: "10.0.0.1", Slot: 9, Input: 1}
	instA2 = demod.Instance{HostIP: "10.0.0.1", Slot: 9, Input: 2}
	instB  = demod.Instance{HostIP: "10.0.0.2", Slot: 9, Input: 1}
	instB2 = demod.Instance{HostIP: "10.0.0.2", Slot: 9, Input: 2}
)

func newHarness(t *testing.T, mod func(*Config), bl fakeBaselines) *harness {
	t.Helper()
	cfg := DefaultConfig
	cfg.Trace.Categories = nil
	if mod != nil {
		mod(&cfg)
	}
	if bl.def.Status == "" {
		bl.def = Baseline{Status: BaselineOK, EffectiveBER: 1e-6, MedianMER: 33}
	}
	h := &harness{t: t, clk: newFakeClock(), duct: &fakeDucting{cd: fresh()}}
	e, err := New(cfg, WithBaselines(bl), WithDucting(h.duct), WithClock(h.clk))
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	t.Cleanup(e.Close)
	return h
}

// report builds a report for inst on LRF TERH RF 10.
func report(inst demod.Instance, side string, locked bool, ber float64, mer, power int) demod.Report {
	r := demod.Report{
		Timestamp: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), HostIP: inst.HostIP, Slot: inst.Slot, Input: inst.Input,
		LRF: "TERH", Side: side, RFChannel: 10, Status: demod.StatusLocked, BER: ber, MER: mer, PowerDBm: power,
	}
	if !locked {
		r.Status, r.MER, r.PowerDBm = "Not Locked", 0, -80
	}
	return r
}

func good(inst demod.Instance, side string) demod.Report { return report(inst, side, true, 0, 33, -40) }
func badBER(inst demod.Instance, side string) demod.Report {
	return report(inst, side, true, 5e-5, 33, -40)
}

// submit hands reports to the engine and waits until they are ingested,
// collecting whatever events and traces come out.
func (h *harness) submit(rs ...demod.Report) {
	h.t.Helper()
	for _, r := range rs {
		if !h.e.Submit(r) {
			h.t.Fatal("submit refused")
		}
		h.n++
	}
	h.wait(func(s Stats) bool { return s.Ingests >= h.n })
}

// minute submits one report per given report, then advances the clock a
// minute (one polling cycle).
func (h *harness) minute(rs ...demod.Report) {
	h.t.Helper()
	h.submit(rs...)
	h.clk.advance(time.Minute)
}

func (h *harness) sweep() {
	h.t.Helper()
	h.clk.ch <- h.clk.Now()
	h.s++
	h.wait(func(s Stats) bool { return s.Sweeps >= h.s })
}

// wait drains events and traces until cond holds and nothing more arrives.
func (h *harness) wait(cond func(Stats) bool) {
	h.t.Helper()
	deadline := time.After(2 * time.Second)
	idle := 0
	for {
		select {
		case ev := <-h.e.Events():
			h.evs = append(h.evs, ev)
			idle = 0
			continue
		case tr := <-h.e.Traces():
			h.trace = append(h.trace, tr)
			idle = 0
			continue
		case <-deadline:
			h.t.Fatal("timed out waiting for the engine")
		case <-time.After(5 * time.Millisecond):
		}
		if cond(h.e.Stats()) {
			idle++
			if idle >= 3 {
				return
			}
		}
	}
}

func (h *harness) states() []State {
	var out []State
	for _, ev := range h.evs {
		out = append(out, ev.State)
	}
	return out
}

func (h *harness) last() Event {
	h.t.Helper()
	if len(h.evs) == 0 {
		h.t.Fatal("no events")
	}
	return h.evs[len(h.evs)-1]
}

func equalStates(a, b []State) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHysteresisEntersAfterThreeAndClearsAfterThree(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	h.minute(badBER(instA, "A"), good(instB, "B"))
	h.minute(badBER(instA, "A"), good(instB, "B"))
	if len(h.evs) != 0 {
		t.Fatalf("event after 2 bad reports: %v", h.states())
	}
	h.minute(badBER(instA, "A"), good(instB, "B"))
	if got := h.states(); !equalStates(got, []State{StateSingleSide}) {
		t.Fatalf("after 3 bad reports states = %v, want [single_side]", got)
	}
	h.minute(good(instA, "A"), good(instB, "B"))
	h.minute(good(instA, "A"), good(instB, "B"))
	if len(h.evs) != 1 {
		t.Fatalf("cleared after 2 clean reports: %v", h.states())
	}
	h.minute(good(instA, "A"), good(instB, "B"))
	if ev := h.last(); ev.Kind != KindClear || ev.Duration != 3*time.Minute || ev.Peak != StateSingleSide {
		t.Fatalf("clear event = %+v", ev)
	}
}

func TestMissingMinuteCountsNeitherWayButLongGapResets(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	h.minute(badBER(instA, "A"), good(instB, "B"))
	h.minute(badBER(instA, "A"), good(instB, "B"))
	h.minute(good(instB, "B")) // A misses a minute
	h.minute(badBER(instA, "A"), good(instB, "B"))
	if got := h.states(); !equalStates(got, []State{StateSingleSide}) {
		t.Fatalf("states = %v, want single_side after a missed minute", got)
	}

	h2 := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	h2.minute(badBER(instA, "A"), good(instB, "B"))
	h2.minute(badBER(instA, "A"), good(instB, "B"))
	h2.clk.advance(4 * time.Minute) // longer than expire_after
	h2.minute(badBER(instA, "A"), good(instB, "B"))
	if len(h2.evs) != 0 {
		t.Fatalf("degraded across a long gap: %v", h2.states())
	}
}

func TestDualWithDuctingConfirmsCCIAndNamesStrongestPath(t *testing.T) {
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("far", ducting.StateTrapping, 150), pathAt("near", ducting.StateTrapping, 80), pathAt("n", ducting.StateNormal, 20)))
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
	}
	ev := h.last()
	if ev.State != StateCCIConfirmed || ev.Kind != KindStart || ev.Strength != ducting.StateTrapping {
		t.Fatalf("event = %s %s strength %s", ev.Kind, ev.State, ev.Strength)
	}
	if ev.Ducting.Strongest == nil || ev.Ducting.Strongest.PathID != "near" || len(ev.Ducting.Active) != 2 || ev.Ducting.PathsOnChannel != 3 {
		t.Fatalf("ducting evidence = %+v", ev.Ducting)
	}
	if ev.Sides[0].View != ViewDegraded || ev.Sides[1].View != ViewDegraded {
		t.Fatalf("sides = %+v", ev.Sides)
	}

	// Trapping weakens to ducting: an update with the new strength.
	h.duct.set(fresh(pathAt("near", ducting.StateDucting, 80)))
	h.minute(badBER(instA, "A"), badBER(instB, "B"))
	if ev := h.last(); ev.Kind != KindUpdate || ev.Strength != ducting.StateDucting {
		t.Fatalf("update = %s strength %s", ev.Kind, ev.Strength)
	}
}

func TestDualVerdictsWithoutDucting(t *testing.T) {
	cases := []struct {
		name   string
		cd     ChannelDucting
		state  State
		reason string
	}{
		{"no paths", fresh(), StateDualUnexplained, "no_cochannel_paths"},
		{"normal paths", fresh(pathAt("p", ducting.StateSuperRefractive, 50)), StateDualUnexplained, "ducting_none_on_channel"},
		{"stale", ChannelDucting{Status: DuctingStale, Paths: []PathEvidence{pathAt("p", ducting.StateTrapping, 50)}}, StateDualDuctingUnknown, "ducting_stale"},
		{"no poll", ChannelDucting{Status: DuctingNoPoll}, StateDualDuctingUnknown, "ducting_no_poll"},
		{"incomplete", fresh(pathAt("p", ducting.StateNormal, 50), pathAt("q", "", 20)), StateDualDuctingUnknown, "ducting_incomplete"},
		{"incomplete but one ducting", fresh(pathAt("p", ducting.StateDucting, 50), pathAt("q", "", 20)), StateCCIConfirmed, "cci_confirmed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil, fakeBaselines{})
			h.duct.set(tc.cd)
			for i := 0; i < 3; i++ {
				h.minute(badBER(instA, "A"), report(instB, "B", true, 0, 28, -40))
			}
			if ev := h.last(); ev.State != tc.state || ev.Reason != tc.reason {
				t.Fatalf("got %s (%s), want %s (%s)", ev.State, ev.Reason, tc.state, tc.reason)
			}
		})
	}
}

func TestStrongestRanksUnknownLast(t *testing.T) {
	cd := fresh(pathAt("unknown-near", "", 10), pathAt("normal-far", ducting.StateNormal, 150))
	if s, _ := cd.Strongest(); s.PathID != "normal-far" {
		t.Errorf("strongest = %s, want the known path", s.PathID)
	}
	if cd.Unknown() != 1 {
		t.Errorf("unknown = %d", cd.Unknown())
	}
}

func TestOneSidedVerdictWaitsForGrace(t *testing.T) {
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateDucting, 50)))
	// A degrades first; B follows a minute later. No single_side episode
	// should appear in between.
	h.minute(badBER(instA, "A"), good(instB, "B"))
	h.minute(badBER(instA, "A"), badBER(instB, "B"))
	h.minute(badBER(instA, "A"), badBER(instB, "B"))
	h.sweep()
	h.minute(badBER(instA, "A"), badBER(instB, "B"))
	if got := h.states(); !equalStates(got, []State{StateCCIConfirmed}) {
		t.Fatalf("states = %v, want only cci_confirmed", got)
	}

	// A alone, with B clean throughout: single_side once the grace passes.
	h2 := newHarness(t, nil, fakeBaselines{})
	for i := 0; i < 5; i++ {
		h2.minute(badBER(instA, "A"), good(instB, "B"))
	}
	if got := h2.states(); !equalStates(got, []State{StateSingleSide}) {
		t.Fatalf("states = %v, want single_side after the grace", got)
	}
}

func TestUnpairedWhenOtherSideMissingOrStale(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"))
	}
	if ev := h.last(); ev.State != StateUnpaired || ev.Reason != "other_side_missing" {
		t.Fatalf("got %s (%s), want unpaired (other_side_missing)", ev.State, ev.Reason)
	}

	h2 := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	h2.minute(good(instA, "A"), good(instB, "B"))
	for i := 0; i < 3; i++ {
		h2.minute(badBER(instA, "A"))
	}
	if ev := h2.last(); ev.State != StateUnpaired || ev.Reason != "other_side_stale" {
		t.Fatalf("got %s (%s), want unpaired (other_side_stale)", ev.State, ev.Reason)
	}
}

func TestLockLossGuard(t *testing.T) {
	// One channel unlocks on both sides while another input on each frame
	// stays locked: CCI through lock loss.
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateTrapping, 50)))
	other := func(inst demod.Instance, side string) demod.Report {
		r := good(inst, side)
		r.RFChannel = 20
		return r
	}
	for i := 0; i < 3; i++ {
		h.minute(report(instA, "A", false, 0, 0, 0), report(instB, "B", false, 0, 0, 0), other(instA2, "A"), other(instB2, "B"))
	}
	if ev := h.last(); ev.State != StateCCIConfirmed {
		t.Fatalf("lock loss on one channel: got %s (%s)", ev.State, ev.Reason)
	}

	// Every input on both frames unlocks: no CCI.
	h2 := newHarness(t, nil, fakeBaselines{})
	h2.duct.set(fresh(pathAt("p", ducting.StateTrapping, 50)))
	for i := 0; i < 4; i++ {
		u := func(inst demod.Instance, side string, rf int) demod.Report {
			r := report(inst, side, false, 0, 0, 0)
			r.RFChannel = rf
			return r
		}
		h2.minute(u(instA, "A", 10), u(instB, "B", 10), u(instA2, "A", 20), u(instB2, "B", 20))
	}
	for _, ev := range h2.evs {
		t.Fatalf("whole frames down produced %s (%s)", ev.State, ev.Reason)
	}
}

func TestFrameReturningRaisesNothing(t *testing.T) {
	// Side A's whole frame goes dark (guarded), then comes back: its inputs
	// are still degraded for 3 reports, but recovering, so no single_side.
	h := newHarness(t, nil, fakeBaselines{})
	u := func(inst demod.Instance, rf int) demod.Report {
		r := report(inst, "A", false, 0, 0, 0)
		r.RFChannel = rf
		return r
	}
	b20 := good(instB2, "B")
	b20.RFChannel = 20
	for i := 0; i < 5; i++ {
		h.minute(u(instA, 10), u(instA2, 20), good(instB, "B"), b20)
		h.sweep()
	}
	a20 := good(instA2, "A")
	a20.RFChannel = 20
	for i := 0; i < 5; i++ {
		h.minute(good(instA, "A"), a20, good(instB, "B"), b20)
		h.sweep()
	}
	if len(h.evs) != 0 {
		t.Fatalf("frame outage and return produced %v", h.states())
	}
}

func TestReturningFrameDoesNotJoinOpenVerdict(t *testing.T) {
	// B is degraded while side A's whole frame is dark (unpaired,
	// guard_frame_down). When A's frame comes back, A is recovering, but it
	// never counted towards the verdict, so it must not make it dual.
	h := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateTrapping, 50)))
	u := func(inst demod.Instance, rf int) demod.Report {
		r := report(inst, "A", false, 0, 0, 0)
		r.RFChannel = rf
		return r
	}
	b20 := good(instB2, "B")
	b20.RFChannel = 20
	for i := 0; i < 4; i++ {
		h.minute(u(instA, 10), u(instA2, 20), badBER(instB, "B"), b20)
		h.sweep()
	}
	if ev := h.last(); ev.State != StateUnpaired || ev.Reason != "guard_frame_down" {
		t.Fatalf("frame down: got %s (%s), want unpaired (guard_frame_down)", ev.State, ev.Reason)
	}
	a20 := good(instA2, "A")
	a20.RFChannel = 20
	for i := 0; i < 4; i++ {
		h.minute(good(instA, "A"), a20, badBER(instB, "B"), b20)
		h.sweep()
	}
	for _, ev := range h.evs {
		if ev.State.dual() {
			t.Fatalf("returning frame made the pair %s (%s); states %v", ev.State, ev.Reason, h.states())
		}
	}
	if ev := h.last(); ev.State != StateSingleSide {
		t.Fatalf("after the frame returned: got %s (%s), want single_side", ev.State, ev.Reason)
	}
}

func TestRecoveringSideDoesNotChangeVerdict(t *testing.T) {
	// B is degraded while side A's whole frame is dark, and both end at
	// once: B is only recovering when A comes back, so the verdict stays
	// unpaired until B clears rather than turning into single_side once
	// pair_grace has passed.
	h := newHarness(t, nil, fakeBaselines{})
	u := func(inst demod.Instance, rf int) demod.Report {
		r := report(inst, "A", false, 0, 0, 0)
		r.RFChannel = rf
		return r
	}
	b20 := good(instB2, "B")
	b20.RFChannel = 20
	for i := 0; i < 6; i++ {
		h.minute(u(instA, 10), u(instA2, 20), badBER(instB, "B"), b20)
		h.sweep()
	}
	a20 := good(instA2, "A")
	a20.RFChannel = 20
	for i := 0; i < 4; i++ {
		h.minute(good(instA, "A"), a20, good(instB, "B"), b20)
		h.sweep()
	}
	if got := h.states(); !equalStates(got, []State{StateUnpaired, StateClear}) {
		t.Fatalf("states = %v, want [unpaired clear]", got)
	}
}

func TestOngoingKeepsReasonDuringGrace(t *testing.T) {
	// cci_confirmed, then side A is clean: single_side waits pair_grace, and
	// an ongoing event in the meantime carries the held state's reason.
	h := newHarness(t, func(c *Config) { c.SnapshotInterval = time.Minute; c.PairGrace = 10 * time.Minute }, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateTrapping, 50)))
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
	}
	for i := 0; i < 5; i++ {
		h.minute(good(instA, "A"), badBER(instB, "B"))
		h.sweep()
	}
	ev := h.last()
	if ev.Kind != KindOngoing || ev.State != StateCCIConfirmed || ev.Reason != "cci_confirmed" {
		t.Fatalf("got %s %s (%s), want ongoing cci_confirmed (cci_confirmed)", ev.Kind, ev.State, ev.Reason)
	}
}

func TestRecoveringSideSustainsOpenVerdict(t *testing.T) {
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateTrapping, 50)))
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
	}
	// Both sides report clean again: cci_confirmed holds until they clear,
	// then goes straight to clear.
	for i := 0; i < 3; i++ {
		h.minute(good(instA, "A"), good(instB, "B"))
	}
	if got := h.states(); !equalStates(got, []State{StateCCIConfirmed, StateClear}) {
		t.Fatalf("states = %v, want [cci_confirmed clear]", got)
	}
}

func TestTwiceReceivedChannelSideDegradedIfEitherInput(t *testing.T) {
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateDucting, 50)))
	for i := 0; i < 3; i++ {
		// Side A receives RF 10 on two inputs; only one is degraded.
		h.minute(badBER(instA, "A"), good(instA2, "A"), badBER(instB, "B"))
	}
	if ev := h.last(); ev.State != StateCCIConfirmed || len(ev.Sides[0].Instances) != 2 {
		t.Fatalf("got %s with %d side A inputs", ev.State, len(ev.Sides[0].Instances))
	}
}

func TestNoBaselineIsSkipped(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{missing: map[demod.Instance]bool{instA: true}})
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), good(instB, "B"))
	}
	if len(h.evs) != 0 {
		t.Fatalf("input without a baseline produced %v", h.states())
	}
	if s := h.e.Stats(); s.Skipped != 3 || s.Evaluated != 3 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestUnusableBaselineSkipReason(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Trace.Categories = []string{"skip"} },
		fakeBaselines{missing: map[demod.Instance]bool{instB: true}, def: Baseline{Status: "insufficient"}})
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
	}
	if len(h.evs) != 0 {
		t.Fatalf("inputs without a usable baseline produced %v", h.states())
	}
	got := map[string]int{}
	for _, tr := range h.trace {
		got[tr.Reason]++
	}
	// One skip record per input, not one per report.
	if got["baseline_insufficient"] != 1 || got["no_baseline"] != 1 || len(h.trace) != 2 {
		t.Fatalf("skip traces = %v", got)
	}
}

func TestOngoingSnapshotsAndClear(t *testing.T) {
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateDucting, 50)))
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
	}
	start := h.last()
	for i := 0; i < 6; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
		h.sweep()
	}
	var ongoing []Event
	for _, ev := range h.evs {
		if ev.Kind == KindOngoing {
			ongoing = append(ongoing, ev)
		}
	}
	if len(ongoing) != 1 || ongoing[0].EpisodeID != start.EpisodeID || ongoing[0].Duration < 5*time.Minute {
		t.Fatalf("ongoing events = %+v", ongoing)
	}
	for i := 0; i < 3; i++ {
		h.minute(good(instA, "A"), good(instB, "B"))
	}
	if ev := h.last(); ev.Kind != KindClear || ev.EpisodeID != start.EpisodeID || ev.Peak != StateCCIConfirmed {
		t.Fatalf("clear = %s episode %s peak %s", ev.Kind, ev.EpisodeID, ev.Peak)
	}
	n := len(h.evs)
	h.clk.advance(10 * time.Minute)
	h.sweep()
	if len(h.evs) != n {
		t.Fatalf("clear pair repeated: %v", h.states())
	}
}

func TestSnapshotIntervalZeroDisablesOngoing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SnapshotInterval = 0 }, fakeBaselines{})
	for i := 0; i < 10; i++ {
		h.minute(badBER(instA, "A"), badBER(instB, "B"))
		h.sweep()
	}
	for _, ev := range h.evs {
		if ev.Kind == KindOngoing {
			t.Fatal("ongoing event with snapshot_interval 0")
		}
	}
}

func TestSignature(t *testing.T) {
	h := newHarness(t, nil, fakeBaselines{})
	h.duct.set(fresh(pathAt("p", ducting.StateDucting, 50)))
	for i := 0; i < 3; i++ {
		h.minute(good(instA, "A"), good(instB, "B"))
	}
	for i := 0; i < 3; i++ {
		// A: MER drop with steady power. B: MER drop with a 10 dB fade.
		h.minute(report(instA, "A", true, 0, 28, -40), report(instB, "B", true, 0, 28, -50))
	}
	ev := h.last()
	if a, b := ev.Sides[0].Instances[0], ev.Sides[1].Instances[0]; a.Signature != SignatureSteady || b.Signature != SignatureDropping || b.PowerDeltaDB != -10 {
		t.Fatalf("signatures A %s, B %s (%v dB)", a.Signature, b.Signature, b.PowerDeltaDB)
	}
}

func TestTraceCategories(t *testing.T) {
	// Disabled categories produce nothing.
	h := newHarness(t, func(c *Config) { c.PairGrace = 0 }, fakeBaselines{})
	for i := 0; i < 3; i++ {
		h.minute(badBER(instA, "A"), good(instB, "B"))
	}
	if len(h.trace) != 0 || h.e.Tracing() {
		t.Fatalf("traces with no categories: %d", len(h.trace))
	}

	h2 := newHarness(t, func(c *Config) {
		c.PairGrace = 0
		c.Trace.Categories = []string{"input", "hysteresis", "pair", "ducting", "skip"}
		c.Trace.LRFSites = []string{"terh"}
	}, fakeBaselines{})
	h2.duct.set(fresh(pathAt("p", ducting.StateTrapping, 50)))
	for i := 0; i < 3; i++ {
		h2.minute(badBER(instA, "A"), badBER(instB, "B"))
	}
	noSide := good(instB2, "")
	h2.minute(noSide)
	reasons := map[string]bool{}
	for _, tr := range h2.trace {
		reasons[string(tr.Category)+"/"+tr.Reason] = true
	}
	for _, want := range []string{"input/ber_spike", "input/ok", "hysteresis/degraded_enter", "hysteresis/stressed_enter", "pair/cci_confirmed", "ducting/path_trapping", "skip/no_side"} {
		if !reasons[want] {
			t.Errorf("missing trace %s; got %v", want, reasons)
		}
	}
	for _, tr := range h2.trace {
		if tr.Category == CategoryPair && tr.Reason == "cci_confirmed" && tr.EpisodeID == "" {
			t.Error("pair trace without an episode ID")
		}
	}

	// Another LRF is filtered out.
	h3 := newHarness(t, func(c *Config) {
		c.Trace.Categories = []string{"input"}
		c.Trace.LRFSites = []string{"PROV"}
	}, fakeBaselines{})
	h3.minute(good(instA, "A"))
	if len(h3.trace) != 0 {
		t.Fatalf("trace for an LRF outside trace.lrf_sites: %+v", h3.trace)
	}
}

func TestTraceBufferDropsWithoutBlocking(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Trace.Categories = []string{"input"}
		c.Trace.Buffer = 2
	}, fakeBaselines{})
	// Don't drain traces: submit directly and wait on ingests only.
	for i := 0; i < 10; i++ {
		if !h.e.Submit(good(instA, "A")) {
			t.Fatal("submit refused")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.e.Stats().Ingests < 10 {
		if time.Now().After(deadline) {
			t.Fatal("engine blocked by an undrained trace buffer")
		}
		time.Sleep(time.Millisecond)
	}
	if d := h.e.Stats().TraceDropped; d < 7 {
		t.Fatalf("dropped %d, want at least 7", d)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	c := DefaultConfig
	c.Trace.Categories = []string{"pair", "bogus"}
	if err := c.Validate(); err == nil {
		t.Fatal("unknown category accepted")
	}
	c = DefaultConfig
	c.BERFactor = 1
	if err := c.Validate(); err == nil {
		t.Fatal("ber_factor 1 accepted")
	}
}
