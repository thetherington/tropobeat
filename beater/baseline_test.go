package beater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v8"

	tropoCfg "github.com/thetherington/tropobeat/config"
	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/correlator"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/lrf"
)

func TestBaselineQuery(t *testing.T) {
	body, err := baselineQuery(7, []string{"TERH", "BOS"}, json.RawMessage(`{"ip":"10.0.0.1","slot":9,"input":1,"rf":10}`))
	if err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	if err := json.Unmarshal([]byte(body), &q); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"gte":"now-7d/d"`, `"lt":"now/d"`, // whole days, nothing from the future
		`"rfdemod.rf_demod.s_input_status":"Locked"`,
		`"rfdemod.rf_demod.s_lrf_id":["TERH","BOS"]`,
		`"hdr":{"number_of_significant_value_digits":3}`,
		`"field":"host.ip"`, `"field":"rfdemod.rf_demod.i_slot"`, `"field":"rfdemod.rf_demod.i_input"`,
		`"after":{"ip":"10.0.0.1","slot":9,"input":1,"rf":10}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("query missing %s:\n%s", want, body)
		}
	}
	// Without sites there is no LRF filter.
	body, _ = baselineQuery(7, nil, nil)
	if strings.Contains(body, `"terms":{"rfdemod.rf_demod.s_lrf_id"`) || strings.Contains(body, `"after"`) {
		t.Errorf("unexpected LRF filter or after key: %s", body)
	}
}

// bucket builds one baseline aggregation bucket as Elasticsearch returns it.
func bucket(ip string, slot, input, rf, docs int, lrfID, side, tag string, ber any, mer map[int]int) map[string]any {
	var merBuckets []any
	for v := 0; v <= 60; v++ {
		if n, ok := mer[v]; ok {
			merBuckets = append(merBuckets, map[string]any{"key": v, "doc_count": n})
		}
	}
	terms := func(k string) map[string]any {
		if k == "" {
			return map[string]any{"buckets": []any{}}
		}
		return map[string]any{"buckets": []any{map[string]any{"key": k, "doc_count": docs}}}
	}
	return map[string]any{
		"key":       map[string]any{"ip": ip, "slot": slot, "input": input, "rf": rf},
		"doc_count": docs,
		"ber":       map[string]any{"values": map[string]any{"50.0": ber}},
		"mer":       map[string]any{"buckets": merBuckets},
		"first":     map[string]any{"value": float64(time.Date(2026, 10, 12, 0, 0, 31, 0, time.UTC).UnixMilli())},
		"last":      map[string]any{"value": float64(time.Date(2026, 10, 18, 23, 59, 10, 0, time.UTC).UnixMilli())},
		"lrf":       terms(lrfID),
		"side":      terms(side),
		"card":      terms(lrfID + " - Demod Card 1 - " + side),
		"frame":     terms("Frame " + side),
		"tags":      terms(tag),
	}
}

// fakeBaselineES serves the baseline aggregation in two pages and records
// the request bodies.
type fakeBaselineES struct {
	mu     sync.Mutex
	pages  [][]any
	bodies []string
}

func (f *fakeBaselineES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	w.Header().Set("Content-Type", "application/json")
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	n := len(f.bodies)
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	keys := map[string]any{"buckets": []any{}}
	if n < len(f.pages) {
		keys = map[string]any{"buckets": f.pages[n], "after_key": map[string]any{"ip": "x", "slot": n, "input": 0, "rf": 0}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"aggregations": map[string]any{"keys": keys}})
}

func baselineES(t *testing.T, f *fakeBaselineES) *elasticsearch.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	es, err := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func testBaselineConfig() tropoCfg.BaselineConfig {
	return tropoCfg.DefaultConfig.Baseline
}

func TestLoadBaselines(t *testing.T) {
	// TERH A 10 from the test cluster: 9,884 samples, MER median 33 dB
	// (the running count crosses 4,942 at 33).
	terhMER := map[int]int{28: 21, 29: 56, 30: 224, 31: 882, 32: 2107, 33: 3745, 34: 2842, 35: 7}
	f := &fakeBaselineES{pages: [][]any{
		{
			bucket("10.91.10.1", 9, 1, 10, 9884, "terh", "A", "WTHI / WTHI2 (OTA)", 0.0, terhMER),
			bucket("10.91.10.2", 9, 1, 10, 9884, "TERH", "1 A", "WTHI (OTA)", 2e-5, map[int]int{30: 10}),
		},
		{
			bucket("10.91.20.1", 10, 2, 37, 9000, "GBAY", "A", "WBAY (OTA)", 16.777215, map[int]int{0: 9000}),
			bucket("10.91.20.1", 10, 3, 37, 9000, "GBAY", "A", "Spare", 0.0, map[int]int{30: 9000}),
			bucket("10.91.20.2", 9, 4, 21, 100, "GBAY", "B", "WFRV (OTA)", 0.0, map[int]int{31: 100}),
			bucket("10.91.20.2", 9, 5, 22, 100, "GBAY", "B", "WGBA (OTA)", nil, map[int]int{31: 100}),
			// No host IP: can't be placed, so it is skipped.
			bucket("", 9, 6, 22, 100, "GBAY", "B", "WGBA (OTA)", 0.0, map[int]int{31: 100}),
		},
	}}
	got, err := loadBaselines(context.Background(), baselineES(t, f), "demod-*", testBaselineConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 3 || !strings.Contains(f.bodies[1], `"after"`) {
		t.Errorf("expected 3 paged requests with after_key, got %d", len(f.bodies))
	}
	if len(got) != 6 {
		t.Fatalf("got %d baselines, want 6", len(got))
	}

	byInst := map[string]Baseline{}
	for _, b := range got {
		byInst[fmt.Sprintf("%s@%d", b.Instance, b.RFChannel)] = b
	}
	terh := byInst["10.91.10.1/9/1@10"]
	if terh.LRF != "TERH" || terh.Side != "A" || terh.Status != BaselineOK || terh.Samples != 9884 {
		t.Errorf("TERH baseline = %+v", terh)
	}
	if terh.MedianBER != 0 || terh.EffectiveBER != 1e-6 || terh.MedianMER != 33 {
		t.Errorf("TERH medians: BER %v (effective %v), MER %v; want 0 (1e-6), 33", terh.MedianBER, terh.EffectiveBER, terh.MedianMER)
	}
	if want := []string{"WTHI", "WTHI2"}; len(terh.CallSigns) != 2 || terh.CallSigns[0] != want[0] || terh.CallSigns[1] != want[1] {
		t.Errorf("TERH call signs = %v", terh.CallSigns)
	}
	if !terh.From.Equal(time.Date(2026, 10, 12, 0, 0, 31, 0, time.UTC)) {
		t.Errorf("TERH From = %v", terh.From)
	}
	if b := byInst["10.91.10.2/9/1@10"]; b.Side != "A" || b.EffectiveBER != 2e-5 {
		t.Errorf(`"1 A" side or effective BER above the floor: %+v`, b)
	}
	for inst, want := range map[string]string{
		"10.91.20.1/10/2@37": BaselineInvalid,      // saturated BER median
		"10.91.20.1/10/3@37": BaselineNoStation,    // "Spare"
		"10.91.20.2/9/4@21":  BaselineInsufficient, // 100 < 1,440 samples
		"10.91.20.2/9/5@22":  BaselineInvalid,      // no BER median
	} {
		if b := byInst[inst]; b.Status != want {
			t.Errorf("%s status = %q, want %q", inst, b.Status, want)
		}
	}
}

func TestMedianOfCounts(t *testing.T) {
	for _, tc := range []struct {
		counts [][2]float64
		want   float64
	}{
		{nil, 0},
		{[][2]float64{{30, 1}}, 30},
		{[][2]float64{{30, 1}, {31, 1}}, 30},
		{[][2]float64{{30, 1}, {31, 2}}, 31},
		{[][2]float64{{28, 21}, {29, 56}, {30, 224}, {31, 882}, {32, 2107}, {33, 3745}, {34, 2842}, {35, 7}}, 33},
	} {
		if got := medianOfCounts(tc.counts); got != tc.want {
			t.Errorf("medianOfCounts(%v) = %v, want %v", tc.counts, got, tc.want)
		}
	}
}

func TestBaselineStore(t *testing.T) {
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	s := newBaselineStore(48 * time.Hour)
	s.now = func() time.Time { return now }
	inst := demod.Instance{HostIP: "10.0.0.1", Slot: 9, Input: 1}

	if _, ok := s.Lookup(inst, 10); ok {
		t.Error("empty store returned a baseline")
	}
	s.swap(&baselineSet{at: now.Add(-time.Hour), byKey: map[baselineKey]Baseline{
		{inst, 10}: {Instance: inst, RFChannel: 10, Status: BaselineOK},
	}})
	if b, ok := s.Lookup(inst, 10); !ok || !b.Usable() {
		t.Errorf("Lookup = %+v, %v", b, ok)
	}
	if _, ok := s.Lookup(inst, 11); ok {
		t.Error("baseline found for another channel")
	}
	if n, at := s.Len(); n != 1 || !at.Equal(now.Add(-time.Hour)) {
		t.Errorf("Len = %d, %v", n, at)
	}
	// Two missed daily runs: too old to use.
	now = now.Add(48 * time.Hour)
	if _, ok := s.Lookup(inst, 10); ok {
		t.Error("stale baselines still returned")
	}
}

func TestBaselineEvent(t *testing.T) {
	at := time.Date(2026, 10, 19, 3, 30, 0, 0, time.UTC)
	b := Baseline{
		Instance: demod.Instance{HostIP: "10.91.10.1", Slot: 9, Input: 1}, RFChannel: 10,
		LRF: "TERH", Side: "A", CardLabel: "TERH - Demod Card 1 - A", FrameLabel: "Terre Haute, IN - A",
		Tags: []string{"WTHI / WTHI2 (OTA)"}, CallSigns: []string{"WTHI", "WTHI2"},
		Samples: 9884, From: at.AddDate(0, 0, -7), To: at.AddDate(0, 0, -7).Add(84 * time.Hour),
		MedianBER: 0, EffectiveBER: 1e-6, MedianMER: 33, Status: BaselineOK,
	}
	site := &lrf.LRF{Code: "TERH", Latitude: 39.465533, Longitude: -87.4023442}
	paths := []cochannel.Path{testPath()}
	f := baselineEvent(b, site, paths, "disc1", "run1", at, correlator.DefaultConfig).Fields
	for k, want := range map[string]any{
		"metricset.name":                           "baseline",
		"tropobeat.baseline.run_id":                "run1",
		"tropobeat.baseline.instance.id":           "10.91.10.1/9/1",
		"tropobeat.baseline.instance.host_ip":      "10.91.10.1",
		"tropobeat.baseline.instance.slot":         9,
		"tropobeat.baseline.lrf.location.lat":      39.465533,
		"tropobeat.baseline.demod_side":            "A",
		"tropobeat.discovery_run_id":               "disc1",
		"tropobeat.baseline.status":                "ok",
		"tropobeat.baseline.samples":               9884,
		"tropobeat.baseline.window.days":           3.5,
		"tropobeat.baseline.ber.median":            0.0,
		"tropobeat.baseline.ber.effective":         1e-6,
		"tropobeat.baseline.mer.median_db":         33.0,
		"tropobeat.baseline.ber.spike_threshold":   correlator.DefaultConfig.BERFactor * b.EffectiveBER,
		"tropobeat.baseline.mer.drop_threshold_db": 30.0,
		"tropobeat.baseline.frame_label":           "Terre Haute, IN - A",
		"tropobeat.baseline.card_label":            "TERH - Demod Card 1 - A",
		"tropobeat.baseline.instance.input":        1,
		"tropobeat.baseline.window.start":          b.From,
		"tropobeat.baseline.lrf.location.lon":      -87.4023442,
	} {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}

	checkLists(t, f, map[string]any{
		"tropobeat.lrf_codes":              []string{"TERH"},
		"tropobeat.rf_channels":            []int{10},
		"tropobeat.path_ids":               []string{"TERH:1001:10"},
		"tropobeat.transmitter_call_signs": []string{"WFAR-TV"},
		"tropobeat.station_call_signs":     []string{"WTHI", "WTHI2"},
	})

	// An LRF not in the LRF list has no location. With no co-channel paths
	// there are no path fields and no discovery run.
	f = baselineEvent(b, nil, nil, "disc1", "run1", at, correlator.DefaultConfig).Fields
	if _, err := f.GetValue("tropobeat.baseline.lrf.location"); err == nil {
		t.Error("location set without an LRF site")
	}
	for _, k := range []string{"tropobeat.path_ids", "tropobeat.transmitter_call_signs", "tropobeat.discovery_run_id"} {
		if _, err := f.GetValue(k); err == nil {
			t.Errorf("%s set without co-channel paths", k)
		}
	}
	checkLists(t, f, map[string]any{"tropobeat.lrf_codes": []string{"TERH"}})
}

func TestDuctingStore(t *testing.T) {
	groups := ductingFixture()
	locs := analyseLocations(groups, ducting.DefaultConfig)
	paths := pathDuctingFrom(groups, locs)
	if len(paths) != 2 {
		t.Fatalf("%d path results, want 2 (the LRF-only group has none)", len(paths))
	}
	if p := paths[0]; p.LRF != "TERH" || p.RFChannel != 10 || len(p.Points) != 3 || !p.MNegative() {
		t.Errorf("path 0 = %+v, want TERH RF 10 with dM/dh < 0 (surface duct at TERH)", p)
	}

	polled := time.Date(2026, 10, 14, 15, 5, 0, 0, time.UTC)
	s := newDuctingStore(time.Hour)
	if _, known := s.MNegative("TERH", 10, polled); known {
		t.Error("empty store reported known")
	}
	s.swap(polled, paths)

	at := polled.Add(30 * time.Minute)
	if neg, known := s.MNegative("TERH", 10, at); !neg || !known {
		t.Errorf("TERH RF 10: neg=%v known=%v, want true, true", neg, known)
	}
	// TERH's duct is at the LRF, which both paths share, so RF 2 is negative too.
	if neg, _ := s.MNegative("TERH", 2, at); !neg {
		t.Error("TERH RF 2 should be negative (shared LRF point)")
	}
	// No co-channel paths on this channel: known, not negative.
	if neg, known := s.MNegative("TERH", 30, at); neg || !known {
		t.Errorf("TERH RF 30: neg=%v known=%v, want false, true", neg, known)
	}
	if neg, known := s.LRFMNegative("TERH", at); !neg || !known {
		t.Errorf("LRFMNegative(TERH) = %v, %v", neg, known)
	}
	if neg, known := s.LRFMNegative("BOS", at); neg || !known {
		t.Errorf("LRFMNegative(BOS) = %v, %v; want false, true", neg, known)
	}
	// Older than 2 × period: unknown.
	if _, known := s.MNegative("TERH", 10, polled.Add(2*time.Hour+time.Minute)); known {
		t.Error("stale ducting still known")
	}
}

func TestIngestStats(t *testing.T) {
	s := newIngestStats()
	r := demod.Report{HostIP: "10.0.0.1", Slot: 9, Input: 1, LRF: "TERH", Status: demod.StatusLocked}
	s.count(r)
	s.count(r)
	r.Input, r.Status = 2, "Not Locked"
	s.count(r)
	s.reject(io.EOF)
	s.reject(nil)
	want := "Demod stream: 3 reports decoded (2 locked) from 2 demod instances at 1 LRFs; 2 rejected (1 malformed, 1 incomplete); 4 reconnects since start"
	if got := s.summary(4); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}
