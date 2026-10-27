package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/thetherington/tropobeat/internal/demod"
)

var t0 = time.Date(2026, 10, 13, 14, 57, 30, 0, time.UTC)

// testDoc builds a report for an input of TERH RF 10.
func testDoc(t *testing.T, at time.Time, ip, side string, rf int) *doc {
	t.Helper()
	src := map[string]any{
		"@timestamp": at.Format(time.RFC3339Nano),
		"host":       map[string]any{"ip": ip},
		"rfdemod": map[string]any{"rf_demod": map[string]any{
			"s_lrf_id": "TERH", "s_demod_side": side, "i_rf_channel": rf, "i_slot": 9, "i_input": 1,
			"s_input_status": "Locked", "i_input_mer": 33, "i_input_power": -40, "d_pre_fec_ber": 0.0,
		}},
	}
	raw := map[string]json.RawMessage{}
	for k, v := range src {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	d, err := newDoc(at, raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// sample is 14 minutes of one input on each side, a report a minute.
func sample(t *testing.T) []*doc {
	var out []*doc
	for m := 0; m < 14; m++ {
		at := t0.Add(time.Duration(m) * time.Minute)
		out = append(out, testDoc(t, at, "10.0.0.1", "A", 10), testDoc(t, at.Add(20*time.Second), "10.0.0.2", "B", 10))
	}
	return out
}

func TestParseScenario(t *testing.T) {
	sc, err := parseScenario([]byte(`
steps:
  - name: cci
    lrf: terh
    rf: 10
    start: 2m
    duration: 10m
    action: ber
  - lrf: TERH
    rf: "*"
    side: a
    start: 15m
    duration: 5m
    action: unlock
`))
	if err != nil {
		t.Fatal(err)
	}
	if sc.repeat != 25*time.Minute {
		t.Errorf("default repeat = %s, want 25m (last end 20m + 5m)", sc.repeat)
	}
	s0, s1 := sc.Steps[0], sc.Steps[1]
	if s0.LRF != "TERH" || s0.rf != 10 || s0.Side != "AB" || s0.Factor != 50 {
		t.Errorf("step 0 = %+v", s0)
	}
	if s1.rf != 0 || s1.Side != "A" || s1.Name != "step 2" {
		t.Errorf("step 1 = %+v", s1)
	}
	if idx, rep := sc.active(27 * time.Minute); len(idx) != 1 || idx[0] != 0 || rep != 1 {
		t.Errorf("active at 27m = %v rep %d, want step 0 in repetition 2", idx, rep)
	}
	if d, ok := sc.next(13 * time.Minute); !ok || d != 2*time.Minute {
		t.Errorf("next at 13m = %s %v", d, ok)
	}

	once, err := parseScenario([]byte("repeat_every: 0s\nsteps: [{lrf: TERH, start: 0s, duration: 1m, action: drop}]"))
	if err != nil || once.repeat != 0 {
		t.Fatalf("run-once scenario: %v, repeat %s", err, once.repeat)
	}
	if idx, _ := once.active(90 * time.Second); len(idx) != 0 {
		t.Error("run-once scenario active after its end")
	}

	for _, bad := range []string{
		"steps: []",
		"steps: [{lrf: TERH, duration: 1m, action: explode}]",
		"steps: [{lrf: TERH, duration: 1m, action: ber, side: C}]",
		"steps: [{duration: 1m, action: ber}]",
		"steps: [{lrf: TERH, duration: 1m, action: ber, colour: red}]",
		"repeat_every: 1m\nsteps: [{lrf: TERH, start: 1m, duration: 1m, action: ber}]",
		"steps: [{lrf: TERH, start: 0s, action: ber}]", // no duration in a repeating scenario
		"steps: [{lrf: TERH, start: 0s, duration: -1m, action: ber}]",
	} {
		if _, err := parseScenario([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRewriterActions(t *testing.T) {
	docs := sample(t)
	stats := sampleStats(docs)
	cases := []struct {
		action string
		check  func(*doc) bool
	}{
		{actionBER, func(d *doc) bool { return math.Abs(d.float(fieldBER)-50e-6) < 1e-12 }},
		{actionSaturate, func(d *doc) bool { return demod.IsSaturatedBER(d.float(fieldBER)) }},
		{actionMER, func(d *doc) bool { return d.int(fieldMER) == 27 && d.int(fieldPower) == -40 }},
		{actionFade, func(d *doc) bool { return d.int(fieldMER) == 27 && d.int(fieldPower) == -46 }},
		{actionUnlock, func(d *doc) bool {
			return d.str(fieldStatus) == "Not Locked" && d.int(fieldMER) == 0 && d.int(fieldPower) == -80
		}},
		{actionNoSide, func(d *doc) bool { return d.str(fieldSide) == "" }},
	}
	for _, tc := range cases {
		sc, err := parseScenario([]byte(`steps: [{lrf: TERH, rf: "10", side: A, start: 0s, duration: 5m, action: ` + tc.action + `}]`))
		if err != nil {
			t.Fatal(err)
		}
		w := newRewriter([]*Scenario{sc}, false, stats)
		a, b := docs[0].clone(), docs[1].clone()
		if !w.apply(a, time.Minute) || !w.apply(b, time.Minute) {
			t.Fatalf("%s: report withheld", tc.action)
		}
		if !tc.check(a) {
			t.Errorf("%s: side A not rewritten: %v", tc.action, a.demod)
		}
		if b.float(fieldBER) != 0 || b.int(fieldMER) != 33 || b.str(fieldSide) != "B" {
			t.Errorf("%s: side B changed: %v", tc.action, b.demod)
		}
		if docs[0].int(fieldMER) != 33 || docs[0].str(fieldStatus) != "Locked" {
			t.Fatalf("%s: the loaded sample was changed", tc.action)
		}
	}

	// drop withholds; outside the step's window nothing changes.
	sc, _ := parseScenario([]byte(`steps: [{lrf: TERH, start: 1m, duration: 1m, action: drop}]`))
	w := newRewriter([]*Scenario{sc}, false, stats)
	if w.apply(docs[0].clone(), 90*time.Second) {
		t.Error("drop step did not withhold")
	}
	if !w.apply(docs[0].clone(), 3*time.Minute) {
		t.Error("report withheld outside the step")
	}

	// flap 2 bad, 2 good.
	sc, _ = parseScenario([]byte(`steps: [{lrf: TERH, side: A, start: 0s, duration: 20m, action: flap, bad: 2, good: 2}]`))
	w = newRewriter([]*Scenario{sc}, false, stats)
	var got []bool
	for i := 0; i < 6; i++ {
		d := docs[0].clone()
		w.apply(d, time.Duration(i)*time.Minute)
		got = append(got, d.float(fieldBER) > 0)
	}
	if want := []bool{true, true, false, false, true, true}; !equalBools(got, want) {
		t.Errorf("flap = %v, want %v", got, want)
	}
}

func TestStepWithoutDurationLastsUntilStopped(t *testing.T) {
	sc, err := parseScenario([]byte("repeat_every: 0s\nsteps: [{lrf: PIT, rf: \"25\", start: 0s, action: ber}]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, time.Hour, 30 * 24 * time.Hour} {
		if idx, _ := sc.active(at); len(idx) != 1 {
			t.Errorf("not active at %s", at)
		}
	}
	if _, ok := sc.next(time.Hour); ok {
		t.Error("a started run-once step reported as next")
	}
}

func TestSeveralScenarios(t *testing.T) {
	docs := sample(t)
	stats := sampleStats(docs)
	constant, _ := parseScenario([]byte("repeat_every: 0s\nsteps: [{lrf: PIT, start: 0s, action: ber}]"))
	cycle, _ := parseScenario([]byte("steps: [{lrf: TERH, side: A, start: 1m, duration: 1m, action: mer}, {lrf: TERH, side: B, start: 1m, duration: 1m, action: ber}]"))
	first, _ := parseScenario([]byte("steps: [{lrf: TERH, side: B, start: 0s, duration: 5m, action: unlock}]"))
	w := newRewriter([]*Scenario{first, cycle, constant}, true, stats)

	a, b := docs[0].clone(), docs[1].clone()
	w.apply(a, 90*time.Second)
	w.apply(b, 90*time.Second)
	if a.int(fieldMER) != 27 {
		t.Errorf("second scenario's step not applied: %v", a.demod)
	}
	if b.str(fieldStatus) != "Not Locked" || b.float(fieldBER) != 0 {
		t.Errorf("first scenario's step should win: %v", b.demod)
	}
	if !w.scenarioLRF("PIT") || !w.scenarioLRF("TERH") || w.scenarioLRF("BOS") {
		t.Error("quiet LRFs are not the union of the scenarios' LRFs")
	}
}

func TestQuietMakesScenarioLRFsHealthy(t *testing.T) {
	docs := sample(t)
	stats := sampleStats(docs)
	sc, _ := parseScenario([]byte(`steps: [{lrf: TERH, side: B, start: 10m, duration: 1m, action: ber}]`))
	w := newRewriter([]*Scenario{sc}, true, stats)
	d := docs[0].clone()
	d.set(fieldBER, 3e-4) // the sample's own spike
	d.set(fieldMER, 25)
	w.apply(d, time.Minute)
	if d.float(fieldBER) != 0 || d.int(fieldMER) != 33 {
		t.Errorf("quiet left %v", d.demod)
	}
}

func equalBools(a, b []bool) bool {
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

// fakePub records published timestamps and fails when told to.
type fakePub struct {
	ts       []time.Time
	failNext int
	connects int
}

func (p *fakePub) connect() error { p.connects++; return nil }
func (p *fakePub) close()         {}
func (p *fakePub) publish(_ context.Context, _ []byte, ts time.Time) error {
	if p.failNext > 0 {
		p.failNext--
		return errors.New("channel closed")
	}
	p.ts = append(p.ts, ts)
	return nil
}

// fakeTime is a clock that sleeping advances instantly.
type fakeTime struct{ t time.Time }

func (f *fakeTime) now() time.Time { return f.t }
func (f *fakeTime) sleep(_ context.Context, d time.Duration) bool {
	if d > 0 {
		f.t = f.t.Add(d)
	}
	return true
}

func TestLoopSeamKeepsCadence(t *testing.T) {
	docs := sample(t)
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clk := &fakeTime{t: start}
	pub := &fakePub{}
	r := &replayer{pub: pub, rw: newRewriter(nil, false, nil), now: clk.now, sleep: clk.sleep,
		opts: replayOptions{shiftNow: true, speed: 1, loop: true, limit: 3 * len(docs)}}
	if err := r.run(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	if len(pub.ts) != 3*len(docs) {
		t.Fatalf("published %d, want %d", len(pub.ts), 3*len(docs))
	}
	// Side A's reports are every other one: across both seams they stay
	// strictly increasing and about a minute apart.
	for i := 2; i < len(pub.ts); i += 2 {
		gap := pub.ts[i].Sub(pub.ts[i-2])
		if gap <= 0 || gap > 2*time.Minute {
			t.Fatalf("report %d: gap %s", i, gap)
		}
	}
	if !pub.ts[0].Equal(start) {
		t.Errorf("first timestamp %s, want the replay start %s (-now)", pub.ts[0], start)
	}
}

func TestReconnectSkipsOverdueReports(t *testing.T) {
	docs := sample(t)
	clk := &fakeTime{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	// A short outage: the reconnect waits about a second, and the next
	// report is 20-40 s away, so only the report that failed is skipped.
	pub := &fakePub{failNext: 1}
	r := &replayer{pub: pub, rw: newRewriter(nil, false, nil), now: clk.now, sleep: clk.sleep,
		opts: replayOptions{shiftNow: true, speed: 1}}
	if err := r.run(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	if pub.connects != 1 || r.skipped != 1 || r.published != len(docs)-1 {
		t.Fatalf("connects %d, skipped %d, published %d", pub.connects, r.skipped, r.published)
	}

	// A long outage: the reports due while disconnected are skipped, not
	// sent in a burst after reconnecting.
	clk2 := &fakeTime{t: clk.t}
	slow := func(ctx context.Context, d time.Duration) bool {
		if d >= reconnectFirst {
			d = 5 * time.Minute // the broker takes 5 minutes to come back
		}
		return clk2.sleep(ctx, d)
	}
	pub2 := &fakePub{failNext: 1}
	r2 := &replayer{pub: pub2, rw: newRewriter(nil, false, nil), now: clk2.now, sleep: slow,
		opts: replayOptions{shiftNow: true, speed: 1}}
	if err := r2.run(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	if r2.skipped < 8 || r2.published+r2.skipped != len(docs) {
		t.Fatalf("skipped %d, published %d of %d", r2.skipped, r2.published, len(docs))
	}
}

func TestShippedScenariosParse(t *testing.T) {
	for _, f := range []string{"scenarios/all-conditions.yml", "scenarios/demo.yml", "scenarios/constant-trapping.yml"} {
		sc, err := loadScenario(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		t.Logf("%s: %d steps, repeats every %s", f, len(sc.Steps), sc.repeat)
	}
}
