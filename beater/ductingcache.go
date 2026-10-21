package beater

import (
	"sync/atomic"
	"time"

	"github.com/thetherington/tropobeat/internal/correlator"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
)

// pathDucting is the latest ducting result along one co-channel path: the
// class and steepest gradient at its LRF, midpoint and transmitter, and the
// path's ducting state for its channel.
type pathDucting struct {
	PathID    string
	LRF       string
	RFChannel int
	// CallSign, FacilityID and DistanceMi describe the path's transmitter,
	// so the correlator can name the likely interferer.
	CallSign   string
	FacilityID string
	DistanceMi float64
	// State is the path's ducting state (normal to trapping), as published
	// on its weather event. Empty when no point of the path has usable
	// weather (the fetch was incomplete or had gaps): unknown, not normal.
	State ducting.State
	// ForecastTime is the hour the weather applies to.
	ForecastTime time.Time
	Points       []pointDucting
}

// pointDucting is the ducting result at one point of a path.
type pointDucting struct {
	Label   string
	Class   ducting.Class
	MinDNdh float64
}

// MNegative reports whether the modified refractivity falls with height
// (dM/dh < 0) somewhere in the point's profile, i.e. it has a ducting layer.
func (p pointDucting) MNegative() bool { return p.Class == ducting.Ducting }

// MNegative reports whether any point along the path has dM/dh < 0.
func (p pathDucting) MNegative() bool {
	for _, pt := range p.Points {
		if pt.MNegative() {
			return true
		}
	}
	return false
}

// ductingSet is one weather poll's path results, replaced as a whole.
type ductingSet struct {
	polledAt  time.Time
	byChannel map[demod.ChannelKey][]pathDucting
	byLRF     map[string][]pathDucting
	paths     int
}

// ductingStore holds the latest ducting result of every co-channel path,
// indexed by LRF and RF channel so a demod report can be matched to the
// paths that could carry interference on its channel. Each weather poll
// swaps in a new set; readers never lock.
type ductingStore struct {
	cur atomic.Pointer[ductingSet]
	// maxAge is how old a poll's results can be before they are treated as
	// unknown: 2 × the weather poll period, so a missed poll is tolerated
	// but stale ducting can never confirm interference.
	maxAge time.Duration
}

func newDuctingStore(period time.Duration) *ductingStore {
	return &ductingStore{maxAge: 2 * period}
}

// swap replaces the ducting results with those of one poll.
func (s *ductingStore) swap(polledAt time.Time, paths []pathDucting) {
	set := &ductingSet{
		polledAt:  polledAt,
		byChannel: map[demod.ChannelKey][]pathDucting{},
		byLRF:     map[string][]pathDucting{},
		paths:     len(paths),
	}
	for _, p := range paths {
		k := demod.ChannelKey{LRF: p.LRF, RFChannel: p.RFChannel}
		set.byChannel[k] = append(set.byChannel[k], p)
		set.byLRF[p.LRF] = append(set.byLRF[p.LRF], p)
	}
	s.cur.Store(set)
}

// fresh returns the current set, or nil when there is none or it is older
// than maxAge at time at.
func (s *ductingStore) fresh(at time.Time) *ductingSet {
	set := s.cur.Load()
	if set == nil || at.Sub(set.polledAt) > s.maxAge {
		return nil
	}
	return set
}

// MNegative reports whether any co-channel path on an LRF's RF channel has
// dM/dh < 0 at any of its points. known is false when there are no recent
// ducting results (no poll yet, or the last one is older than 2 × period);
// a channel with no co-channel paths is known and not negative.
func (s *ductingStore) MNegative(lrf string, rfChannel int, at time.Time) (neg, known bool) {
	set := s.fresh(at)
	if set == nil {
		return false, false
	}
	for _, p := range set.byChannel[demod.ChannelKey{LRF: lrf, RFChannel: rfChannel}] {
		if p.MNegative() {
			return true, true
		}
	}
	return false, true
}

// Channel returns the ducting of every co-channel path on an LRF's RF
// channel for the correlator, with whether the results are fresh, stale or
// not yet available.
func (s *ductingStore) Channel(lrf string, rfChannel int, at time.Time) correlator.ChannelDucting {
	set := s.cur.Load()
	if set == nil {
		return correlator.ChannelDucting{Status: correlator.DuctingNoPoll}
	}
	cd := correlator.ChannelDucting{Status: correlator.DuctingFresh, PolledAt: set.polledAt}
	if at.Sub(set.polledAt) > s.maxAge {
		cd.Status = correlator.DuctingStale
	}
	for _, p := range set.byChannel[demod.ChannelKey{LRF: lrf, RFChannel: rfChannel}] {
		cd.ForecastTime = p.ForecastTime
		ev := correlator.PathEvidence{
			PathID: p.PathID, CallSign: p.CallSign, FacilityID: p.FacilityID,
			DistanceMi: p.DistanceMi, State: p.State,
		}
		for _, pt := range p.Points {
			if pt.MNegative() {
				ev.DuctingPoints = append(ev.DuctingPoints, pt.Label)
			}
		}
		cd.Paths = append(cd.Paths, ev)
	}
	return cd
}

// LRFMNegative reports whether any co-channel path of an LRF, on any
// channel, has dM/dh < 0. known is as for MNegative.
func (s *ductingStore) LRFMNegative(lrf string, at time.Time) (neg, known bool) {
	set := s.fresh(at)
	if set == nil {
		return false, false
	}
	for _, p := range set.byLRF[lrf] {
		if p.MNegative() {
			return true, true
		}
	}
	return false, true
}

// pathDuctingFrom builds the ducting results of the path groups of a weather
// poll from the analysed locations. LRF-only groups have no path and are
// skipped. Path groups whose weather is missing or unusable are kept with an
// empty State, so the correlator sees the path as unknown rather than the
// channel as having no co-channel paths.
func pathDuctingFrom(groups []fetchedGroup, locs map[string]*location) []pathDucting {
	out := make([]pathDucting, 0, len(groups))
	for _, fg := range groups {
		if fg.path == nil {
			continue
		}
		pd := pathDucting{
			PathID:     fg.group.ID,
			LRF:        demod.NormalizeLRF(fg.path.LRFCode),
			RFChannel:  fg.path.RFChannel,
			CallSign:   fg.path.Transmitter.CallSign,
			FacilityID: fg.path.Transmitter.FacilityID,
			DistanceMi: fg.path.DistanceMi,
		}
		var analyses []ducting.Analysis
		for _, pt := range fg.group.Points {
			if pt.Profile == nil {
				continue
			}
			pd.ForecastTime = pt.Profile.Time
			loc := locs[profileID(pt.Profile)]
			if loc == nil || !loc.analysis.Usable() {
				continue
			}
			analyses = append(analyses, loc.analysis)
			pd.Points = append(pd.Points, pointDucting{Label: pt.Label, Class: loc.analysis.Class, MinDNdh: loc.analysis.MinDNdh})
		}
		// The same state as the path's weather event: trapping needs the
		// channel's frequency, so a non-TV channel is never trapping.
		var mhz []float64
		if f, ok := ducting.ChannelMHz(fg.path.RFChannel); ok {
			mhz = []float64{f}
		}
		if st, ok := ducting.StateOf(analyses, mhz); ok {
			pd.State = st
		}
		out = append(out, pd)
	}
	return out
}
