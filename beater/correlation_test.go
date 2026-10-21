package beater

import (
	"testing"
	"time"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/correlator"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/lrf"
)

func TestDuctingStoreChannel(t *testing.T) {
	groups := ductingFixture()
	locs := analyseLocations(groups, ducting.DefaultConfig)
	paths := pathDuctingFrom(groups, locs)

	// The path state matches the one published on the path's weather event.
	ev := weatherEvent(groups[0].group, groups[0].path, "TERH", "run", time.Now(), locs)
	if want := get(t, ev.Fields, "tropobeat.ducting_state"); string(paths[0].State) != want {
		t.Errorf("path state %s, weather event state %v", paths[0].State, want)
	}
	if p := paths[0]; p.CallSign != "WFAR-TV" || p.FacilityID != "1001" || p.DistanceMi != 99.5 {
		t.Errorf("transmitter fields = %+v", p)
	}

	s := newDuctingStore(time.Hour)
	polled := time.Date(2026, 10, 14, 15, 5, 0, 0, time.UTC)
	if cd := s.Channel("TERH", 10, polled); cd.Status != correlator.DuctingNoPoll {
		t.Errorf("empty store status %s", cd.Status)
	}
	s.swap(polled, paths)

	cd := s.Channel("TERH", 10, polled.Add(30*time.Minute))
	if cd.Status != correlator.DuctingFresh || len(cd.Paths) != 1 || !cd.PolledAt.Equal(polled) {
		t.Fatalf("TERH RF 10 = %+v", cd)
	}
	p := cd.Paths[0]
	if p.PathID != "TERH:1001:10" || p.CallSign != "WFAR-TV" || p.State.Level() < ducting.StateDucting.Level() {
		t.Errorf("path evidence = %+v", p)
	}
	if len(p.DuctingPoints) != 1 || p.DuctingPoints[0] != cochannel.LabelLRF {
		t.Errorf("ducting points = %v, want [lrf] (the surface duct at TERH)", p.DuctingPoints)
	}
	if cd := s.Channel("TERH", 30, polled); cd.Status != correlator.DuctingFresh || len(cd.Paths) != 0 {
		t.Errorf("channel with no paths = %+v", cd)
	}
	if cd := s.Channel("TERH", 10, polled.Add(2*time.Hour+time.Minute)); cd.Status != correlator.DuctingStale || cd.Known() {
		t.Errorf("stale = %+v", cd)
	}
}

func TestPathDuctingIncompleteIsUnknown(t *testing.T) {
	groups := ductingFixture()
	// Path 0 lost its weather entirely.
	for i := range groups[0].group.Points {
		groups[0].group.Points[i].Profile = nil
	}
	locs := analyseLocations(groups[1:], ducting.DefaultConfig)
	paths := pathDuctingFrom(groups, locs)
	if len(paths) != 2 || paths[0].State != "" || len(paths[0].Points) != 0 {
		t.Fatalf("incomplete path = %+v, want kept with an unknown state", paths[0])
	}
	s := newDuctingStore(time.Hour)
	at := time.Date(2026, 10, 14, 15, 5, 0, 0, time.UTC)
	s.swap(at, paths)
	if cd := s.Channel("TERH", 10, at); len(cd.Paths) != 1 || cd.Unknown() != 1 {
		t.Errorf("channel = %+v, want one unknown path", cd)
	}
}

func TestCorrelatorBaselinesAdapter(t *testing.T) {
	inst := demod.Instance{HostIP: "10.0.0.1", Slot: 9, Input: 1}
	s := newBaselineStore(48 * time.Hour)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	a := correlatorBaselines{s}
	if _, ok := a.Lookup(inst, 10); ok {
		t.Fatal("lookup before any baseline run")
	}
	s.swap(&baselineSet{runID: "run1", at: now.Add(-time.Hour), byKey: map[baselineKey]Baseline{
		{inst, 10}: {Status: BaselineOK, EffectiveBER: 1e-6, MedianMER: 33},
	}})
	if b, ok := a.Lookup(inst, 10); !ok || !b.Usable() || b.MedianMER != 33 || b.RunID != "run1" {
		t.Errorf("lookup = %+v, %v", b, ok)
	}
	if _, ok := a.Lookup(inst, 11); ok {
		t.Error("lookup on another channel")
	}
	now = now.Add(48 * time.Hour)
	if b, ok := a.Lookup(inst, 10); !ok || b.Status != baselineStale || b.Usable() {
		t.Errorf("stale lookup = %+v, %v", b, ok)
	}
}

func testSnapshot() *snapshot {
	return &snapshot{
		RunID: "disc1",
		LRFs:  []lrf.LRF{{Code: "TERH", Latitude: 39.465533, Longitude: -87.4023442}},
		Paths: []cochannel.Path{testPath()},
	}
}

func TestCorrelationEvent(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	strongest := correlator.PathEvidence{PathID: "TERH:1001:10", CallSign: "WFAR-TV", FacilityID: "1001", DistanceMi: 99.5, State: ducting.StateTrapping, DuctingPoints: []string{"midpoint"}}
	side := func(s string, ber float64) correlator.SideEvidence {
		return correlator.SideEvidence{Side: s, View: correlator.ViewDegraded, Since: at.Add(-3 * time.Minute), Instances: []correlator.InstanceEvidence{{
			Instance: demod.Instance{HostIP: "10.0.0." + s, Slot: 9, Input: 1}, Side: s, Fresh: true, Locked: true,
			BER: ber, MER: 30, PowerDBm: -40, EffectiveBER: 1e-6, BERThreshold: 1e-5, MedianMER: 33, MERThreshold: 30,
			Conditions: []string{correlator.CondBERSpike, correlator.CondMERDrop}, Degraded: true, Signature: correlator.SignatureSteady,
		}}}
	}
	ev := correlator.Event{
		Kind: correlator.KindStart, At: at, LRF: "TERH", RFChannel: 10,
		State: correlator.StateCCIConfirmed, Previous: correlator.StateClear, Reason: "cci_confirmed", Strength: ducting.StateTrapping,
		EpisodeID: "TERH:10:20261021T120000Z", EpisodeStart: at, Peak: correlator.StateCCIConfirmed,
		Sides:   []correlator.SideEvidence{side("A", 5e-5), side("B", 4e-5)},
		Ducting: correlator.DuctingEvidence{Status: correlator.DuctingFresh, PolledAt: at, PathsOnChannel: 1, Strongest: &strongest, Active: []correlator.PathEvidence{strongest}},
	}
	f := correlationEvent(ev, testSnapshot()).Fields
	for k, want := range map[string]any{
		"metricset.name":                                    "correlation",
		"tropobeat.correlation.kind":                        "start",
		"tropobeat.correlation.state":                       "cci_confirmed",
		"tropobeat.correlation.previous_state":              "clear",
		"tropobeat.correlation.severity":                    5,
		"tropobeat.correlation.strength":                    "trapping",
		"tropobeat.correlation.episode.id":                  "TERH:10:20261021T120000Z",
		"tropobeat.correlation.lrf.location.lat":            39.465533,
		"tropobeat.correlation.sides.a.view":                "degraded",
		"tropobeat.correlation.sides.a.ber":                 5e-5,
		"tropobeat.correlation.sides.b.mer_db":              30,
		"tropobeat.correlation.ducting.status":              "fresh",
		"tropobeat.correlation.ducting.strongest.call_sign": "WFAR-TV",
		"tropobeat.correlation.ducting.strongest.level":     3,
		"tropobeat.ducting_state":                           "trapping",
		"tropobeat.discovery_run_id":                        "disc1",
	} {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	checkLists(t, f, map[string]any{
		"tropobeat.lrf_codes":                             []string{"TERH"},
		"tropobeat.rf_channels":                           []int{10},
		"tropobeat.path_ids":                              []string{"TERH:1001:10"},
		"tropobeat.transmitter_call_signs":                []string{"WFAR-TV"},
		"tropobeat.correlation.sides.a.conditions":        []string{"ber_spike", "mer_drop"},
		"tropobeat.correlation.ducting.active_call_signs": []string{"WFAR-TV"},
	})

	// An LRF missing from the LRF list: no location, still the LRF code.
	ev.LRF = "MON"
	f = correlationEvent(ev, testSnapshot()).Fields
	if _, err := f.GetValue("tropobeat.correlation.lrf"); err == nil {
		t.Error("location for an LRF not in the LRF list")
	}
	checkLists(t, f, map[string]any{"tropobeat.lrf_codes": []string{"MON"}})
}

func TestCorrelationTraceEvent(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tr := correlator.Trace{
		Category: correlator.CategoryHysteresis, At: at, Reason: "degraded_enter", Decision: correlator.DecisionHit,
		Message: "3 anomalous reports in a row", LRF: "TERH", RFChannel: 10, EpisodeID: "ep1",
		Input: &correlator.InstanceEvidence{Instance: demod.Instance{HostIP: "10.0.0.1", Slot: 9, Input: 1}, Side: "A", EffectiveBER: 1e-6, BERThreshold: 1e-5, Bad: 3},
	}
	f := correlationTraceEvent(tr, testSnapshot()).Fields
	for k, want := range map[string]any{
		"metricset.name":                                  "correlation_trace",
		"tropobeat.correlation_trace.category":            "hysteresis",
		"tropobeat.correlation_trace.reason":              "degraded_enter",
		"tropobeat.correlation_trace.decision":            "hit",
		"tropobeat.correlation_trace.episode_id":          "ep1",
		"tropobeat.correlation_trace.input.id":            "10.0.0.1/9/1",
		"tropobeat.correlation_trace.input.bad":           3,
		"tropobeat.correlation_trace.input.ber_threshold": 1e-5,
		"tropobeat.correlation_trace.lrf.location.lon":    -87.4023442,
	} {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}

	// A sweep summary has no LRF.
	sw := correlator.Trace{Category: correlator.CategorySweep, At: at, Reason: "summary", Sweep: &correlator.SweepSummary{Instances: 4, Pairs: 2}}
	f = correlationTraceEvent(sw, testSnapshot()).Fields
	if got := get(t, f, "tropobeat.correlation_trace.sweep.instances"); got != 4 {
		t.Errorf("sweep.instances = %v", got)
	}
	if _, err := f.GetValue("tropobeat.lrf_codes"); err == nil {
		t.Error("sweep summary has LRF codes")
	}
}

func TestCorrelatorSummary(t *testing.T) {
	s := correlator.Stats{Evaluated: 120, Skipped: 5, Instances: 40, Events: 3, Traces: 9,
		Pairs: map[correlator.State]int64{correlator.StateClear: 18, correlator.StateCCIConfirmed: 2}}
	prev := correlator.Stats{Evaluated: 20, Events: 1}
	want := "Correlator: 100 reports judged, 5 skipped (no usable baseline); 40 inputs live; pairs not clear: 2 cci_confirmed; 2 events, 9 trace records published, 0 trace records dropped"
	if got := correlatorSummary(s, prev); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}
