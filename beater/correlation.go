package beater

import (
	"fmt"
	"strings"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/correlator"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/lrf"
)

// correlatorDebugSelector is the logp selector for trace records written to
// the log (correlator.trace.log). Enable with `logging.level: debug` and
// `logging.selectors: ["correlator"]`, or `-d correlator`.
const correlatorDebugSelector = "correlator"

// newCorrelator starts the correlation engine on the baseline and ducting
// stores.
func newCorrelator(cfg correlator.Config, baselines *baselineStore, ducting *ductingStore) (*correlator.Engine, error) {
	return correlator.New(cfg,
		correlator.WithBaselines(correlatorBaselines{baselines}),
		correlator.WithDucting(ducting),
		correlator.WithClock(correlator.RealClock{}),
	)
}

// snapshotIndex is built from a snapshot on first use, so the live stream
// can find an LRF's location and a channel's co-channel paths without
// scanning every path for every event.
type snapshotIndex struct {
	lrfs      map[string]lrf.LRF
	byChannel map[demod.ChannelKey][]cochannel.Path
}

func (s *snapshot) index() *snapshotIndex {
	s.indexOnce.Do(func() {
		s.idx = &snapshotIndex{lrfs: map[string]lrf.LRF{}, byChannel: map[demod.ChannelKey][]cochannel.Path{}}
		for _, l := range s.LRFs {
			s.idx.lrfs[demod.NormalizeLRF(l.Code)] = l
		}
		for _, p := range s.Paths {
			k := demod.ChannelKey{LRF: demod.NormalizeLRF(p.LRFCode), RFChannel: p.RFChannel}
			s.idx.byChannel[k] = append(s.idx.byChannel[k], p)
		}
	})
	return s.idx
}

// correlationKeys returns the shared fields of a correlation or trace
// event about an LRF and channel: the LRF, the channel and every co-channel
// path on it, so the Ducting Watch filters apply. It also returns the LRF's
// location field, when the LRF is in the LRF list. rf 0 (a sweep summary)
// gives only the LRF, if any.
func correlationKeys(snap *snapshot, lrfCode string, rf int) (keys, mapstr.M) {
	k := keys{}
	if lrfCode != "" {
		k.lrfCodes = []string{lrfCode}
	}
	if rf != 0 {
		k.rfChannels = []int{rf}
	}
	if snap == nil || lrfCode == "" {
		return k, nil
	}
	idx := snap.index()
	paths := idx.byChannel[demod.ChannelKey{LRF: lrfCode, RFChannel: rf}]
	for i := range paths {
		k.addPath(&paths[i])
	}
	if len(paths) > 0 {
		k.discoveryRunID = snap.RunID
	}
	var loc mapstr.M
	if l, ok := idx.lrfs[lrfCode]; ok {
		loc = mapstr.M{"location": mapstr.M{"lat": l.Latitude, "lon": l.Longitude}}
	}
	return k, loc
}

// correlationEvent describes a verdict event: a pair's CCI verdict
// starting, changing, re-published while ongoing, or clearing.
func correlationEvent(ev correlator.Event, snap *snapshot) beat.Event {
	k, loc := correlationKeys(snap, ev.LRF, ev.RFChannel)
	if s := ev.Ducting.Strongest; s != nil && s.Known() && ev.Ducting.Status == correlator.DuctingFresh {
		k.state, k.hasState = ev.Ducting.Strongest.State, true
	}
	episode := mapstr.M{}
	if ev.EpisodeID != "" {
		episode["id"] = ev.EpisodeID
		episode["start"] = ev.EpisodeStart
		episode["duration_s"] = ev.Duration.Seconds()
		episode["peak_state"] = string(ev.Peak)
	}
	c := mapstr.M{
		"kind":     string(ev.Kind),
		"state":    string(ev.State),
		"severity": ev.State.Severity(),
		"reason":   ev.Reason,
		"episode":  episode,
		"sides":    sidesFields(ev.Sides),
		"ducting":  ductingEvidenceFields(ev.Ducting),
	}
	if ev.Previous != "" && ev.Kind != correlator.KindOngoing {
		c["previous_state"] = string(ev.Previous)
	}
	if ev.Strength != "" {
		c["strength"] = string(ev.Strength)
	}
	if loc != nil {
		c["lrf"] = loc
	}
	t := k.fields()
	t["correlation"] = c
	return beat.Event{
		Timestamp: ev.At,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetCorrelation},
			"tropobeat": t,
		},
	}
}

// correlationTraceEvent describes one decision-trace record.
func correlationTraceEvent(tr correlator.Trace, snap *snapshot) beat.Event {
	k, loc := correlationKeys(snap, tr.LRF, tr.RFChannel)
	c := mapstr.M{
		"category": string(tr.Category),
		"reason":   tr.Reason,
		"decision": tr.Decision,
		"message":  tr.Message,
	}
	if tr.EpisodeID != "" {
		c["episode_id"] = tr.EpisodeID
	}
	if tr.State != "" {
		c["state"] = string(tr.State)
	}
	if tr.Input != nil {
		c["input"] = instanceFields(*tr.Input)
	}
	if len(tr.Sides) > 0 {
		c["sides"] = sidesFields(tr.Sides)
	}
	if tr.Ducting != nil {
		c["ducting"] = ductingEvidenceFields(*tr.Ducting)
	}
	if s := tr.Sweep; s != nil {
		c["sweep"] = mapstr.M{
			"instances":     s.Instances,
			"expired":       s.Expired,
			"pairs":         s.Pairs,
			"not_clear":     s.NotClear,
			"reevaluated":   s.Reevaluated,
			"trace_dropped": s.TraceDropped,
		}
	}
	if loc != nil {
		c["lrf"] = loc
	}
	t := k.fields()
	t["correlation_trace"] = c
	return beat.Event{
		Timestamp: tr.At,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetCorrelationTrace},
			"tropobeat": t,
		},
	}
}

// sidesFields gives side A as sides.a and side B as sides.b: the side's
// view, when its degradation began, a summary across its inputs (the
// conditions, worst BER, MER and power, and signature) and every input.
func sidesFields(sides []correlator.SideEvidence) mapstr.M {
	out := mapstr.M{}
	for _, s := range sides {
		m := mapstr.M{"view": s.View}
		if !s.Since.IsZero() {
			m["since"] = s.Since
		}
		var conds, ids, sigs []string
		insts := make([]mapstr.M, 0, len(s.Instances))
		for _, ie := range s.Instances {
			insts = append(insts, instanceFields(ie))
			ids = append(ids, ie.Instance.String())
			if !ie.Fresh {
				continue
			}
			conds = append(conds, ie.Conditions...)
			sigs = append(sigs, ie.Signature)
			// Worst readings across the side's fresh inputs.
			if v, ok := m["ber"].(float64); !ok || ie.BER > v {
				m["ber"] = ie.BER
			}
			if v, ok := m["mer_db"].(int); !ok || ie.MER < v {
				m["mer_db"] = ie.MER
			}
			if v, ok := m["power_dbm"].(int); !ok || ie.PowerDBm < v {
				m["power_dbm"] = ie.PowerDBm
			}
		}
		putList(m, "conditions", conds)
		putList(m, "instance_ids", ids)
		putList(m, "signatures", sigs)
		m["instances"] = insts
		switch s.Side {
		case "A":
			out["a"] = m
		case "B":
			out["b"] = m
		}
	}
	return out
}

// instanceFields describes one demod input's latest reading and judgement.
func instanceFields(ie correlator.InstanceEvidence) mapstr.M {
	m := mapstr.M{
		"id":          ie.Instance.String(),
		"host_ip":     ie.Instance.HostIP,
		"slot":        ie.Instance.Slot,
		"input":       ie.Instance.Input,
		"side":        ie.Side,
		"card_label":  ie.CardLabel,
		"frame_label": ie.FrameLabel,
		"fresh":       ie.Fresh,
		"locked":      ie.Locked,
		"ber":         ie.BER,
		"mer_db":      ie.MER,
		"power_dbm":   ie.PowerDBm,
		"degraded":    ie.Degraded,
		"stressed":    ie.Stressed,
		"bad":         ie.Bad,
		"good":        ie.Good,
	}
	if !ie.ReportTime.IsZero() {
		m["report_time"] = ie.ReportTime
	}
	// Inputs without a usable baseline (skip records) have no thresholds.
	if ie.EffectiveBER > 0 {
		m["ber_effective"] = ie.EffectiveBER
		m["ber_threshold"] = ie.BERThreshold
		m["mer_median_db"] = ie.MedianMER
		m["mer_threshold_db"] = ie.MERThreshold
	}
	putList(m, "conditions", ie.Conditions)
	if ie.Signature != "" {
		m["signature"] = ie.Signature
		m["power_delta_db"] = ie.PowerDeltaDB
	}
	return m
}

// ductingEvidenceFields describes the ducting looked up for a pair.
func ductingEvidenceFields(d correlator.DuctingEvidence) mapstr.M {
	m := mapstr.M{"status": d.Status, "paths_on_channel": d.PathsOnChannel, "paths_unknown": d.PathsUnknown}
	if !d.PolledAt.IsZero() {
		m["polled_at"] = d.PolledAt
	}
	if !d.ForecastTime.IsZero() {
		m["forecast_time"] = d.ForecastTime
	}
	if s := d.Strongest; s != nil && s.Known() {
		st := mapstr.M{
			"path_id":     s.PathID,
			"call_sign":   s.CallSign,
			"facility_id": s.FacilityID,
			"distance_mi": s.DistanceMi,
			"state":       string(s.State),
			"level":       s.State.Level(),
		}
		putList(st, "ducting_points", s.DuctingPoints)
		m["strongest"] = st
	}
	var ids, calls []string
	for _, p := range d.Active {
		ids = append(ids, p.PathID)
		calls = append(calls, p.CallSign)
	}
	putList(m, "active_path_ids", ids)
	putList(m, "active_call_signs", calls)
	return m
}

// logTrace writes a trace record to the log at debug level
// (correlator.trace.log).
func logTrace(log *logp.Logger, tr correlator.Trace) {
	kv := []interface{}{
		"category", string(tr.Category), "reason", tr.Reason, "decision", tr.Decision,
		"lrf", tr.LRF, "rf_channel", tr.RFChannel,
	}
	if tr.EpisodeID != "" {
		kv = append(kv, "episode_id", tr.EpisodeID)
	}
	if tr.Input != nil {
		kv = append(kv, "instance", tr.Input.Instance.String(), "side", tr.Input.Side)
	}
	log.Debugw(tr.Message, kv...)
}

// correlatorSummary is the correlator line of the minute ingest summary:
// counts since the previous summary (prev), and the pairs in each state now.
func correlatorSummary(s, prev correlator.Stats) string {
	var pairs []string
	for _, st := range correlator.States[1:] {
		if n := s.Pairs[st]; n > 0 {
			pairs = append(pairs, fmt.Sprintf("%d %s", n, st))
		}
	}
	notClear := "none"
	if len(pairs) > 0 {
		notClear = strings.Join(pairs, ", ")
	}
	return fmt.Sprintf("Correlator: %d reports judged, %d skipped (no usable baseline); %d inputs live; pairs not clear: %s; %d events, %d trace records published, %d trace records dropped",
		s.Evaluated-prev.Evaluated, s.Skipped-prev.Skipped, s.Instances, notClear,
		s.Events-prev.Events, s.Traces-prev.Traces, s.TraceDropped-prev.TraceDropped)
}

// logEvent logs a verdict event, except ongoing re-publishes.
func logEvent(log *logp.Logger, ev correlator.Event) {
	if ev.Kind == correlator.KindOngoing {
		return
	}
	msg := fmt.Sprintf("Correlation %s RF %d: %s %s (%s)", ev.LRF, ev.RFChannel, ev.Kind, ev.State, ev.Reason)
	if s := ev.Ducting.Strongest; s != nil && ev.State == correlator.StateCCIConfirmed {
		msg += fmt.Sprintf("; likely interferer %s (%s, %.0f mi, %s)", s.CallSign, s.PathID, s.DistanceMi, s.State)
	}
	if ev.Kind == correlator.KindClear {
		msg += fmt.Sprintf("; lasted %s, peak %s", ev.Duration.Round(time.Second), ev.Peak)
	}
	log.Info(msg)
}
