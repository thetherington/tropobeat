package beater

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/go-elasticsearch/v8"

	tropoCfg "github.com/thetherington/tropobeat/config"
	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/lrf"
)

// Baseline statuses. Only BaselineOK baselines are used to judge live
// telemetry; the others are kept (and published) so they can be seen.
const (
	// BaselineOK has enough locked reports and a real BER median.
	BaselineOK = "ok"
	// BaselineInsufficient has fewer than baseline.min_samples reports.
	BaselineInsufficient = "insufficient"
	// BaselineInvalid has a saturated BER median (demod.BERSaturated), as
	// spare inputs that report "Locked" do.
	BaselineInvalid = "invalid"
	// BaselineNoStation is an input whose tags name no station ("Spare",
	// "NA"), so there is nothing to protect.
	BaselineNoStation = "no_station"
)

// Baseline is the normal Pre-FEC BER and MER of one demod instance on one RF
// channel: the medians of its locked reports over the baseline window.
type Baseline struct {
	Instance   demod.Instance
	RFChannel  int
	LRF        string
	Side       string
	CardLabel  string
	FrameLabel string
	Tags       []string
	CallSigns  []string

	// Samples is the number of locked reports the medians cover, and From
	// and To the first and last of them.
	Samples int
	From    time.Time
	To      time.Time

	// MedianBER is the median Pre-FEC BER. EffectiveBER is the value a
	// spike is measured against: max(MedianBER, baseline.ber_floor).
	MedianBER    float64
	EffectiveBER float64
	// MedianMER is the median MER in dB (the demods report whole dB).
	MedianMER float64

	Status string
}

// Usable reports whether the baseline can be used to judge live telemetry.
func (b Baseline) Usable() bool { return b.Status == BaselineOK }

// baselineKey identifies a baseline: a demod instance on one RF channel. The
// channel is part of the key so an input retuned within the window gets a
// baseline per channel instead of a blend of two stations.
type baselineKey struct {
	instance  demod.Instance
	rfChannel int
}

// baselineSet is one baseline run's results, replaced as a whole.
type baselineSet struct {
	runID string
	at    time.Time
	byKey map[baselineKey]Baseline
}

// baselineStore holds the latest baselines. Each run builds a new set and
// swaps it in, so readers never lock and always see one complete run; a
// failed run keeps the previous set.
type baselineStore struct {
	cur    atomic.Pointer[baselineSet]
	maxAge time.Duration
	now    func() time.Time
}

func newBaselineStore(maxAge time.Duration) *baselineStore {
	return &baselineStore{maxAge: maxAge, now: time.Now}
}

// swap replaces the baselines with set.
func (s *baselineStore) swap(set *baselineSet) { s.cur.Store(set) }

// Lookup returns the baseline of a demod instance on an RF channel. It
// reports false when there is none, or when the baselines are older than
// baseline.max_age (several runs have failed). Check Usable before using
// the result.
func (s *baselineStore) Lookup(inst demod.Instance, rfChannel int) (Baseline, bool) {
	set := s.cur.Load()
	if set == nil || s.now().Sub(set.at) > s.maxAge {
		return Baseline{}, false
	}
	b, ok := set.byKey[baselineKey{inst, rfChannel}]
	return b, ok
}

// Len returns the number of baselines held, and when they were computed.
func (s *baselineStore) Len() (int, time.Time) {
	set := s.cur.Load()
	if set == nil {
		return 0, time.Time{}
	}
	return len(set.byKey), set.at
}

// runBaseline computes the baselines of every demod instance from the last
// baseline.window_days of demod telemetry, swaps them into memory and
// publishes them. On error the previous baselines are kept.
func (bt *tropobeat) runBaseline(ctx context.Context) error {
	start := time.Now().UTC()
	runID := start.Format("20060102T150405Z")
	cfg := bt.config.Baseline

	// With lrf_sites set, limit the query to those LRFs. The snapshot holds
	// the selected LRFs (seeded in Run, refreshed by discovery).
	var (
		lrfs           []lrf.LRF
		paths          []cochannel.Path
		discoveryRunID string
	)
	if snap := bt.state.Load(); snap != nil {
		lrfs, paths, discoveryRunID = snap.LRFs, snap.Paths, snap.RunID
	}
	var sites []string
	if len(bt.config.LRFSites) > 0 {
		for _, l := range lrfs {
			sites = append(sites, demod.NormalizeLRF(l.Code))
		}
	}

	baselines, err := loadBaselines(ctx, bt.es, bt.config.DemodIndex, cfg, sites)
	if err != nil {
		return err
	}
	set := &baselineSet{runID: runID, at: start, byKey: make(map[baselineKey]Baseline, len(baselines))}
	for _, b := range baselines {
		set.byKey[baselineKey{b.Instance, b.RFChannel}] = b
	}
	bt.baselines.swap(set)

	if cfg.Publish {
		locs := make(map[string]lrf.LRF, len(lrfs))
		for _, l := range lrfs {
			locs[demod.NormalizeLRF(l.Code)] = l
		}
		// Each baseline lists the co-channel paths on its LRF and channel
		// from the latest discovery run. At start-up the first baseline run
		// can finish before discovery, and then lists none.
		byChannel := map[demod.ChannelKey][]cochannel.Path{}
		for _, p := range paths {
			k := demod.ChannelKey{LRF: demod.NormalizeLRF(p.LRFCode), RFChannel: p.RFChannel}
			byChannel[k] = append(byChannel[k], p)
		}
		events := make([]beat.Event, 0, len(baselines))
		for _, b := range baselines {
			var site *lrf.LRF
			if l, ok := locs[b.LRF]; ok {
				site = &l
			}
			onChannel := byChannel[demod.ChannelKey{LRF: b.LRF, RFChannel: b.RFChannel}]
			events = append(events, baselineEvent(b, site, onChannel, discoveryRunID, runID, start))
		}
		bt.client.PublishAll(events)
	}

	// Summary log: how many baselines of each status, and the time span the
	// demod data covers.
	counts := map[string]int{}
	var oldest time.Time
	for _, b := range baselines {
		counts[b.Status]++
		if !b.From.IsZero() && (oldest.IsZero() || b.From.Before(oldest)) {
			oldest = b.From
		}
	}
	bt.logger.Infof("Baseline run %s: %d demod instance baselines (%d ok, %d insufficient, %d invalid, %d no station) from %d days of %s; published: %v (%s)",
		runID, len(baselines), counts[BaselineOK], counts[BaselineInsufficient], counts[BaselineInvalid], counts[BaselineNoStation],
		cfg.WindowDays, bt.config.DemodIndex, cfg.Publish, time.Since(start).Round(time.Millisecond))

	// Warn when the demod indices don't reach back over the whole window
	// (a day of slack for the first report of the first day).
	windowStart := start.Truncate(24*time.Hour).AddDate(0, 0, -cfg.WindowDays)
	if len(baselines) == 0 {
		bt.logger.Warnf("Baseline run %s: no locked demod reports in the last %d days of %s", runID, cfg.WindowDays, bt.config.DemodIndex)
	} else if oldest.After(windowStart.Add(24 * time.Hour)) {
		bt.logger.Warnf("Baseline run %s: demod data only goes back to %s, less than the %d-day window; check the retention of %s",
			runID, oldest.Format(time.RFC3339), cfg.WindowDays, bt.config.DemodIndex)
	}
	return nil
}

// loadBaselines runs the baseline aggregation over index and returns one
// baseline per demod instance and RF channel, sorted by instance. sites,
// when not empty, limits it to those LRF codes.
func loadBaselines(ctx context.Context, es *elasticsearch.Client, index string, cfg tropoCfg.BaselineConfig, sites []string) ([]Baseline, error) {
	var (
		out   []Baseline
		after json.RawMessage
	)
	for {
		body, err := baselineQuery(cfg.WindowDays, sites, after)
		if err != nil {
			return nil, err
		}
		res, err := es.Search(
			es.Search.WithContext(ctx),
			es.Search.WithIndex(index),
			es.Search.WithBody(strings.NewReader(body)),
		)
		if err != nil {
			return nil, fmt.Errorf("querying demod baselines in %s: %w", index, err)
		}
		var page baselinePage
		// Decode, then close straight away: this runs in a loop.
		decodeErr := json.NewDecoder(res.Body).Decode(&page)
		isErr, status := res.IsError(), res.String()
		res.Body.Close()
		if isErr {
			return nil, fmt.Errorf("querying demod baselines in %s: %s", index, status)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding demod baselines from %s: %w", index, decodeErr)
		}

		buckets := page.Aggregations.Keys.Buckets
		for _, b := range buckets {
			if bl, ok := b.baseline(cfg); ok {
				out = append(out, bl)
			}
		}
		if len(buckets) == 0 || len(page.Aggregations.Keys.AfterKey) == 0 {
			break
		}
		after = page.Aggregations.Keys.AfterKey
	}
	slices.SortFunc(out, func(a, b Baseline) int {
		if c := strings.Compare(a.LRF, b.LRF); c != 0 {
			return c
		}
		if c := strings.Compare(a.Instance.String(), b.Instance.String()); c != 0 {
			return c
		}
		return a.RFChannel - b.RFChannel
	})
	return out, nil
}

// baselineQuery builds one page of the baseline aggregation: per demod
// instance (host IP, slot, input) and RF channel, the median BER and MER of
// its locked reports over the last windowDays whole UTC days, plus the
// attributes needed to describe it. No documents are returned. For a 7-day
// window the first page is:
//
//	{
//	  "size": 0,
//	  "query": { "bool": { "filter": [
//	    { "range": { "@timestamp": { "gte": "now-7d/d", "lt": "now/d" } } },
//	    { "term": { "rfdemod.rf_demod.s_input_status": "Locked" } }
//	  ] } },
//	  "aggs": { "keys": {
//	    "composite": { "size": 1000, "sources": [
//	      { "ip":    { "terms": { "field": "host.ip" } } },
//	      { "slot":  { "terms": { "field": "rfdemod.rf_demod.i_slot" } } },
//	      { "input": { "terms": { "field": "rfdemod.rf_demod.i_input" } } },
//	      { "rf":    { "terms": { "field": "rfdemod.rf_demod.i_rf_channel" } } }
//	    ] },
//	    "aggs": {
//	      "ber":   { "percentiles": { "field": "rfdemod.rf_demod.d_pre_fec_ber", "percents": [50],
//	                                  "hdr": { "number_of_significant_value_digits": 3 } } },
//	      "mer":   { "terms": { "field": "rfdemod.rf_demod.i_input_mer", "size": 100, "order": { "_key": "asc" } } },
//	      "first": { "min": { "field": "@timestamp" } },
//	      "last":  { "max": { "field": "@timestamp" } },
//	      "lrf":   { "terms": { "field": "rfdemod.rf_demod.s_lrf_id", "size": 1 } },
//	      "side":  { "terms": { "field": "rfdemod.rf_demod.s_demod_side", "size": 1 } },
//	      "card":  { "terms": { "field": "rfdemod.rf_demod.s_card_label", "size": 1 } },
//	      "frame": { "terms": { "field": "rfdemod.rf_demod.s_frame_label", "size": 1 } },
//	      "tags":  { "terms": { "field": "rfdemod.rf_demod.s_input_tag", "size": 5 } }
//	    }
//	  } }
//	}
//
// The window ends at the start of today ("lt": "now/d"), so the result is the
// same all day and demod indices dated in the future are never included.
// The BER median uses HDR histograms because the default (TDigest)
// interpolates between BER's 1e-6 steps. The MER median is worked out
// exactly from the per-dB counts (see medianOfCounts). With sites set, a
// terms filter on the LRF ID is added. Later pages add the previous page's
// after_key to "composite".
func baselineQuery(windowDays int, sites []string, after json.RawMessage) (string, error) {
	terms := func(name, field string) map[string]any {
		return map[string]any{name: map[string]any{"terms": map[string]any{"field": field}}}
	}
	composite := map[string]any{
		"size": compositeSize,
		"sources": []any{
			terms("ip", demod.FieldHostIP),
			terms("slot", demod.FieldSlot),
			terms("input", demod.FieldInput),
			terms("rf", demod.FieldRFChannel),
		},
	}
	if len(after) > 0 {
		composite["after"] = after
	}
	top := func(field string, size int) map[string]any {
		return map[string]any{"terms": map[string]any{"field": field, "size": size}}
	}
	filter := []any{
		map[string]any{"range": map[string]any{demod.FieldTimestamp: map[string]any{
			"gte": fmt.Sprintf("now-%dd/d", windowDays),
			"lt":  "now/d",
		}}},
		map[string]any{"term": map[string]any{demod.FieldInputStatus: demod.StatusLocked}},
	}
	if len(sites) > 0 {
		filter = append(filter, map[string]any{"terms": map[string]any{demod.FieldLRFID: sites}})
	}
	q := map[string]any{
		"size":  0,
		"query": map[string]any{"bool": map[string]any{"filter": filter}},
		"aggs": map[string]any{"keys": map[string]any{
			"composite": composite,
			"aggs": map[string]any{
				"ber": map[string]any{"percentiles": map[string]any{
					"field":    demod.FieldPreFECBER,
					"percents": []float64{50},
					"hdr":      map[string]any{"number_of_significant_value_digits": 3},
				}},
				"mer": map[string]any{"terms": map[string]any{
					"field": demod.FieldMER, "size": 100, "order": map[string]any{"_key": "asc"},
				}},
				"first": map[string]any{"min": map[string]any{"field": demod.FieldTimestamp}},
				"last":  map[string]any{"max": map[string]any{"field": demod.FieldTimestamp}},
				"lrf":   top(demod.FieldLRFID, 1),
				"side":  top(demod.FieldSide, 1),
				"card":  top(demod.FieldCardLabel, 1),
				"frame": top(demod.FieldFrameLabel, 1),
				"tags":  top(demod.FieldInputTag, 5),
			},
		}},
	}
	b, err := json.Marshal(q)
	return string(b), err
}

// compositeSize is the number of baseline buckets per page.
const compositeSize = 1000

// baselinePage is the part of a baseline aggregation response that is
// decoded.
type baselinePage struct {
	Aggregations struct {
		Keys struct {
			AfterKey json.RawMessage  `json:"after_key"`
			Buckets  []baselineBucket `json:"buckets"`
		} `json:"keys"`
	} `json:"aggregations"`
}

type termsAgg[T any] struct {
	Buckets []struct {
		Key      T   `json:"key"`
		DocCount int `json:"doc_count"`
	} `json:"buckets"`
}

// first returns the first bucket's key, or the zero value.
func (t termsAgg[T]) first() T {
	var zero T
	if len(t.Buckets) == 0 {
		return zero
	}
	return t.Buckets[0].Key
}

type valueAgg struct {
	Value *float64 `json:"value"`
}

// time returns the value as a time (Elasticsearch returns dates as epoch
// milliseconds), or the zero time.
func (v valueAgg) time() time.Time {
	if v.Value == nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(*v.Value)).UTC()
}

type baselineBucket struct {
	Key struct {
		IP    *string `json:"ip"`
		Slot  *int    `json:"slot"`
		Input *int    `json:"input"`
		RF    *int    `json:"rf"`
	} `json:"key"`
	DocCount int `json:"doc_count"`
	BER      struct {
		Values map[string]*float64 `json:"values"`
	} `json:"ber"`
	MER   termsAgg[float64] `json:"mer"`
	First valueAgg          `json:"first"`
	Last  valueAgg          `json:"last"`
	LRF   termsAgg[string]  `json:"lrf"`
	Side  termsAgg[string]  `json:"side"`
	Card  termsAgg[string]  `json:"card"`
	Frame termsAgg[string]  `json:"frame"`
	Tags  termsAgg[string]  `json:"tags"`
}

// baseline turns a bucket into a Baseline and sets its status. It reports
// false for buckets with an incomplete key, which can't be placed.
func (b baselineBucket) baseline(cfg tropoCfg.BaselineConfig) (Baseline, bool) {
	k := b.Key
	if k.IP == nil || *k.IP == "" || k.Slot == nil || k.Input == nil || k.RF == nil {
		return Baseline{}, false
	}
	bl := Baseline{
		Instance:   demod.Instance{HostIP: *k.IP, Slot: *k.Slot, Input: *k.Input},
		RFChannel:  *k.RF,
		LRF:        demod.NormalizeLRF(b.LRF.first()),
		Side:       demod.NormalizeSide(b.Side.first()),
		CardLabel:  b.Card.first(),
		FrameLabel: b.Frame.first(),
		Samples:    b.DocCount,
		From:       b.First.time(),
		To:         b.Last.time(),
	}

	// Distinct tags (normalised) and the call signs in them.
	calls := map[string]struct{}{}
	for _, t := range b.Tags.Buckets {
		tag := cochannel.NormalizeTag(t.Key)
		if tag != "" && !slices.Contains(bl.Tags, tag) {
			bl.Tags = append(bl.Tags, tag)
		}
		for _, c := range cochannel.ParseCallSigns(tag) {
			calls[c] = struct{}{}
		}
	}
	for c := range calls {
		bl.CallSigns = append(bl.CallSigns, c)
	}
	slices.Sort(bl.CallSigns)

	// The BER median is null when no report had a BER; treat that like a
	// saturated reading, as there is nothing to compare against.
	berKnown := false
	if v := b.BER.Values["50.0"]; v != nil {
		bl.MedianBER, berKnown = *v, true
	}
	bl.EffectiveBER = max(bl.MedianBER, cfg.BERFloor)

	counts := make([][2]float64, 0, len(b.MER.Buckets))
	for _, m := range b.MER.Buckets {
		counts = append(counts, [2]float64{m.Key, float64(m.DocCount)})
	}
	bl.MedianMER = medianOfCounts(counts)

	switch {
	case len(bl.CallSigns) == 0:
		bl.Status = BaselineNoStation
	case !berKnown || demod.IsSaturatedBER(bl.MedianBER):
		bl.Status = BaselineInvalid
	case bl.Samples < cfg.MinSamples:
		bl.Status = BaselineInsufficient
	default:
		bl.Status = BaselineOK
	}
	return bl, true
}

// medianOfCounts returns the median of a distribution given as (value,
// count) pairs sorted by value: the first value at which the running count
// reaches half the total. It returns 0 for an empty distribution.
func medianOfCounts(counts [][2]float64) float64 {
	var total float64
	for _, c := range counts {
		total += c[1]
	}
	var acc float64
	for _, c := range counts {
		acc += c[1]
		if acc*2 >= total {
			return c[0]
		}
	}
	return 0
}
