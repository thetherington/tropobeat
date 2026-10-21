package correlator

import (
	"time"

	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
)

// Baseline is what the engine needs from a demod input's 7-day baseline.
type Baseline struct {
	// Status is "ok" when the baseline can be used. Anything else (such as
	// "insufficient", "invalid", "no_station", or "stale" when the
	// baselines are too old) is reported as the reason the input was
	// skipped.
	Status       string
	EffectiveBER float64
	MedianMER    float64
	RunID        string
}

// BaselineOK is the Status of a usable baseline.
const BaselineOK = "ok"

// Usable reports whether the baseline can judge live telemetry.
func (b Baseline) Usable() bool { return b.Status == BaselineOK }

// Baselines looks up the baseline of a demod input on an RF channel. ok is
// false when there is none.
type Baselines interface {
	Lookup(inst demod.Instance, rfChannel int) (b Baseline, ok bool)
}

// Ducting statuses (ChannelDucting.Status).
const (
	// DuctingFresh: the latest weather poll is recent enough to use.
	DuctingFresh = "fresh"
	// DuctingStale: the latest poll is older than 2 × the poll period.
	DuctingStale = "stale"
	// DuctingNoPoll: no weather poll has finished yet.
	DuctingNoPoll = "no_poll"
)

// ChannelDucting is the latest ducting result of every co-channel path on
// one LRF and RF channel.
type ChannelDucting struct {
	Status       string
	PolledAt     time.Time
	ForecastTime time.Time
	// Paths are every co-channel path on the channel, ducting or not. Empty
	// for a channel with no co-channel paths.
	Paths []PathEvidence
}

// Known reports whether the ducting is fresh enough to decide on.
func (c ChannelDucting) Known() bool { return c.Status == DuctingFresh }

// Strongest returns the path with the highest ducting state, the nearest
// transmitter winning a tie; a path with unknown ducting ranks below every
// known one. ok is false when there are no paths.
func (c ChannelDucting) Strongest() (p PathEvidence, ok bool) {
	for i, x := range c.Paths {
		if i == 0 || x.level() > p.level() || (x.level() == p.level() && x.DistanceMi < p.DistanceMi) {
			p = x
		}
	}
	return p, len(c.Paths) > 0
}

// Unknown counts the paths whose ducting is unknown.
func (c ChannelDucting) Unknown() int {
	n := 0
	for _, p := range c.Paths {
		if !p.Known() {
			n++
		}
	}
	return n
}

// PathEvidence is one co-channel path's latest ducting result.
type PathEvidence struct {
	PathID     string
	CallSign   string
	FacilityID string
	DistanceMi float64
	State      ducting.State
	// DuctingPoints are the labels (lrf, midpoint, transmitter) of the
	// points with dM/dh < 0.
	DuctingPoints []string
}

// Known reports whether the path's ducting is known: empty State means no
// point of the path had usable weather.
func (p PathEvidence) Known() bool { return p.State != "" }

func (p PathEvidence) level() int {
	if !p.Known() {
		return -1
	}
	return p.State.Level()
}

// Ducting looks up the ducting on the co-channel paths of an LRF and RF
// channel, as of time at.
type Ducting interface {
	Channel(lrf string, rfChannel int, at time.Time) ChannelDucting
}

// State is the verdict for one LRF and RF channel (its A/B pair).
type State string

// Pair states, in increasing order of severity (see Severity).
const (
	// StateClear: no side is degraded.
	StateClear State = "clear"
	// StateSingleSide: one side is degraded and the other is clean, which
	// points to a demod or equipment fault rather than the received signal.
	StateSingleSide State = "single_side"
	// StateUnpaired: one side is degraded and the other can't be judged
	// (none at this LRF, no recent report, or its frame is down).
	StateUnpaired State = "unpaired"
	// StateDualDuctingUnknown: both sides are degraded, but there are no
	// recent ducting results.
	StateDualDuctingUnknown State = "dual_ducting_unknown"
	// StateDualUnexplained: both sides are degraded, and no co-channel path
	// on the channel has dM/dh < 0.
	StateDualUnexplained State = "dual_unexplained"
	// StateCCIConfirmed: both sides are degraded, and a co-channel path on
	// the channel is ducting or trapping.
	StateCCIConfirmed State = "cci_confirmed"
)

// States lists every pair state, in order of severity.
var States = []State{StateClear, StateSingleSide, StateUnpaired, StateDualDuctingUnknown, StateDualUnexplained, StateCCIConfirmed}

// Severity returns the state's position in States (clear 0 to
// cci_confirmed 5).
func (s State) Severity() int {
	for i, x := range States {
		if x == s {
			return i
		}
	}
	return 0
}

// dual reports whether both sides are degraded in this state.
func (s State) dual() bool { return s.Severity() >= StateDualDuctingUnknown.Severity() }

// oneSided reports whether the state is a one-sided verdict, which waits
// for PairGrace.
func (s State) oneSided() bool { return s == StateSingleSide || s == StateUnpaired }

// EventKind says why an event was emitted.
type EventKind string

// Event kinds.
const (
	// KindStart: a pair left clear; a new episode starts.
	KindStart EventKind = "start"
	// KindUpdate: the state changed within an episode, or (while
	// cci_confirmed) the ducting evidence changed.
	KindUpdate EventKind = "update"
	// KindOngoing: a periodic re-publish of a non-clear pair.
	KindOngoing EventKind = "ongoing"
	// KindClear: the pair returned to clear; the episode ends.
	KindClear EventKind = "clear"
)

// Side views (SideEvidence.View).
const (
	ViewDegraded = "degraded"
	ViewClean    = "clean"
	// ViewGuarded: degraded only by lock loss while its whole frame is
	// unlocked, so it is not treated as a signal problem.
	ViewGuarded = "guarded"
	// ViewRecovering: still degraded (not yet clean for HysteresisCycles
	// reports), but every degraded input's latest report is clean. It keeps
	// an open verdict going but never starts one, so a frame coming back
	// after an outage, or the tail of an episode, raises nothing new.
	ViewRecovering = "recovering"
	// ViewStale: the side's inputs have no recent report.
	ViewStale = "stale"
	// ViewMissing: no input on that side is known for the channel.
	ViewMissing = "missing"
)

// Signatures (InstanceEvidence.Signature).
const (
	SignatureSteady   = "steady_or_rising"
	SignatureDropping = "dropping"
	SignatureUnlocked = "unlocked"
)

// Event describes a pair's verdict. It is emitted when the verdict starts,
// changes, is re-published (ongoing) or clears.
type Event struct {
	Kind      EventKind
	At        time.Time
	LRF       string
	RFChannel int
	State     State
	Previous  State
	// Reason is the rule that decided the state (see the pair reasons).
	Reason string
	// Strength is the ducting state of the strongest path when the state is
	// cci_confirmed (ducting or trapping).
	Strength ducting.State

	EpisodeID    string
	EpisodeStart time.Time
	// Duration is how long the episode has lasted (ongoing and clear).
	Duration time.Duration
	// Peak is the most severe state reached in the episode.
	Peak State

	// Sides are side A then side B.
	Sides   []SideEvidence
	Ducting DuctingEvidence
}

// SideEvidence is one side of a pair at the time of an event or trace.
type SideEvidence struct {
	Side string
	View string
	// Since is when the side's earliest current degradation began.
	Since     time.Time
	Instances []InstanceEvidence
}

// InstanceEvidence is one demod input's latest reading and judgement.
type InstanceEvidence struct {
	Instance   demod.Instance
	Side       string
	CardLabel  string
	FrameLabel string
	ReportTime time.Time
	Fresh      bool

	Locked   bool
	BER      float64
	MER      int
	PowerDBm int

	EffectiveBER float64
	BERThreshold float64
	MedianMER    float64
	MERThreshold float64

	// Conditions active on the latest report: ber_spike, ber_saturated,
	// mer_drop, lock_lost.
	Conditions []string
	Degraded   bool
	Stressed   bool
	// Bad and Good are the consecutive anomalous and clean report counts.
	Bad, Good int

	Signature    string
	PowerDeltaDB float64
}

// DuctingEvidence is the ducting looked up for a pair.
type DuctingEvidence struct {
	Status         string
	PolledAt       time.Time
	ForecastTime   time.Time
	PathsOnChannel int
	// PathsUnknown is how many of them have no usable weather.
	PathsUnknown int
	// Strongest is the path with the highest ducting state, if any.
	Strongest *PathEvidence
	// Active are the paths that are ducting or trapping.
	Active []PathEvidence
}

// Trace decisions.
const (
	DecisionHit  = "hit"
	DecisionMiss = "miss"
	DecisionSkip = "skip"
)

// Trace is one decision-trace record: how the engine judged a report, an
// input, a pair or the ducting, and why.
type Trace struct {
	Category Category
	At       time.Time
	// Reason is a short code (see CORRELATION.md); Message explains it.
	Reason   string
	Decision string
	Message  string

	LRF       string
	RFChannel int
	EpisodeID string

	// Input is set for input, skip and hysteresis records.
	Input *InstanceEvidence
	// State and Sides are set for pair records.
	State State
	Sides []SideEvidence
	// Ducting is set for pair and ducting records.
	Ducting *DuctingEvidence
	// Sweep is set for sweep records.
	Sweep *SweepSummary
}

// SweepSummary is what one sweep did.
type SweepSummary struct {
	Instances    int
	Expired      int
	Pairs        int
	NotClear     int
	Reevaluated  int
	TraceDropped int64
}
