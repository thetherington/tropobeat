package correlator

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Config configures the correlation engine (the tropobeat.correlator
// section).
type Config struct {
	// Enabled runs the engine on the live demod stream. It only has an
	// effect when rabbitmq.enabled is set.
	Enabled bool `config:"enabled"`
	// Publish publishes the engine's events as the correlation metricset.
	Publish bool `config:"publish"`

	// BERFactor: a locked report is a BER spike when its Pre-FEC BER is at
	// least BERFactor × the baseline's effective BER (or saturated).
	BERFactor float64 `config:"ber_factor"`
	// MERDropDB: a locked report is an MER drop when its MER is at least
	// MERDropDB below the baseline median.
	MERDropDB float64 `config:"mer_drop_db"`
	// LockLoss counts an unlocked report from a normally locked input as an
	// anomaly, like a BER spike or MER drop.
	LockLoss bool `config:"lock_loss"`
	// HysteresisCycles is how many consecutive anomalous reports make an
	// input degraded, and how many consecutive clean ones clear it.
	HysteresisCycles int `config:"hysteresis_cycles"`
	// PowerDropDB: an input whose power falls at least PowerDropDB below its
	// pre-onset level has a "dropping" signature (a fade) rather than
	// "steady_or_rising" (typical of interference).
	PowerDropDB float64 `config:"power_drop_db"`

	// StaleAfter is how old an input's last report can be and still count
	// for its side of a pair.
	StaleAfter time.Duration `config:"stale_after"`
	// ExpireAfter: an input with no report for this long loses its live
	// state, and a gap this long between two reports restarts its counters.
	ExpireAfter time.Duration `config:"expire_after"`
	// PairGrace is how long a one-sided verdict (single_side or unpaired)
	// must hold before it is reported, so the other side's own hysteresis
	// can catch up at the start and end of an A+B episode.
	PairGrace time.Duration `config:"pair_grace"`
	// SnapshotInterval is how often an ongoing (non-clear) pair is
	// re-published with its current evidence. 0 turns it off.
	SnapshotInterval time.Duration `config:"snapshot_interval"`
	// SweepInterval is how often the engine checks for silent inputs,
	// stale sides, grace periods, ducting changes and snapshots.
	SweepInterval time.Duration `config:"sweep_interval"`
	// InputBuffer is the depth of the report queue into the engine.
	InputBuffer int `config:"input_buffer"`

	Trace TraceConfig `config:"trace"`
}

// TraceConfig configures the decision trace: records of how the engine
// reached (or didn't reach) each verdict, published as the
// correlation_trace metricset.
type TraceConfig struct {
	// Categories lists the trace categories to publish (see Categories).
	Categories CategoryList `config:"categories"`
	// LRFSites limits input, skip, hysteresis, pair and ducting records to
	// these LRF codes. Empty means every LRF.
	LRFSites []string `config:"lrf_sites"`
	// Buffer is how many trace records can wait to be published. Beyond it
	// records are dropped (and counted), so tracing never slows the engine.
	Buffer int `config:"buffer"`
	// Log also writes every trace record to the tropobeat log at debug
	// level (selector "correlator").
	Log bool `config:"log"`
}

// DefaultConfig is the default engine configuration.
var DefaultConfig = Config{
	Enabled:          true,
	Publish:          true,
	BERFactor:        10,
	MERDropDB:        3,
	LockLoss:         true,
	HysteresisCycles: 3,
	PowerDropDB:      3,
	StaleAfter:       90 * time.Second,
	ExpireAfter:      3 * time.Minute,
	PairGrace:        2 * time.Minute,
	SnapshotInterval: 5 * time.Minute,
	SweepInterval:    10 * time.Second,
	InputBuffer:      1024,
	Trace: TraceConfig{
		Categories: CategoryList{string(CategoryPair), string(CategoryHysteresis)},
		Buffer:     4096,
	},
}

// Validate checks the configuration.
func (c Config) Validate() error {
	var errs []error
	if c.BERFactor <= 1 {
		errs = append(errs, fmt.Errorf("correlator.ber_factor must be greater than 1, got %v", c.BERFactor))
	}
	if c.MERDropDB <= 0 {
		errs = append(errs, errors.New("correlator.mer_drop_db must be positive"))
	}
	if c.PowerDropDB <= 0 {
		errs = append(errs, errors.New("correlator.power_drop_db must be positive"))
	}
	if c.HysteresisCycles < 1 {
		errs = append(errs, errors.New("correlator.hysteresis_cycles must be at least 1"))
	}
	for name, d := range map[string]time.Duration{
		"stale_after": c.StaleAfter, "expire_after": c.ExpireAfter, "sweep_interval": c.SweepInterval,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("correlator.%s must be positive", name))
		}
	}
	if c.ExpireAfter < c.StaleAfter {
		errs = append(errs, errors.New("correlator.expire_after must be at least stale_after"))
	}
	if c.PairGrace < 0 || c.SnapshotInterval < 0 {
		errs = append(errs, errors.New("correlator.pair_grace and correlator.snapshot_interval must not be negative"))
	}
	if c.InputBuffer <= 0 || c.Trace.Buffer <= 0 {
		errs = append(errs, errors.New("correlator.input_buffer and correlator.trace.buffer must be positive"))
	}
	if _, err := parseCategories(c.Trace.Categories); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Category is a kind of trace record.
type Category string

// Trace categories.
const (
	// CategoryInput: every report evaluated against its baseline.
	CategoryInput Category = "input"
	// CategorySkip: a report that could not be evaluated (no usable
	// baseline, no side label), at most once per input per hour.
	CategorySkip Category = "skip"
	// CategoryHysteresis: an input becoming or ceasing to be degraded or
	// stressed, a gap restarting its counters, or its live state expiring.
	CategoryHysteresis Category = "hysteresis"
	// CategoryPair: an A/B pair decision, when it is not clear or changes.
	CategoryPair Category = "pair"
	// CategoryDucting: the ducting looked up for a pair with a degraded
	// side, path by path.
	CategoryDucting Category = "ducting"
	// CategorySweep: a summary of each sweep.
	CategorySweep Category = "sweep"
)

// Categories lists every trace category.
var Categories = []Category{CategoryInput, CategorySkip, CategoryHysteresis, CategoryPair, CategoryDucting, CategorySweep}

// CategoryList is a list of trace category names. A list set in the
// configuration replaces the default list as a whole: without its own
// unpacking, the configuration library would merge a configured list into
// the default one element by element ([input] over [pair, hysteresis]
// giving [input, hysteresis]), and an empty list could not turn tracing off.
type CategoryList []string

// Unpack sets the list from a configured list (or a single name).
func (l *CategoryList) Unpack(v interface{}) error {
	switch x := v.(type) {
	case nil:
		*l = CategoryList{}
	case string:
		*l = CategoryList{x}
	case []interface{}:
		out := make(CategoryList, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("correlator.trace.categories: %v is not a category name", e)
			}
			out = append(out, s)
		}
		*l = out
	default:
		return fmt.Errorf("correlator.trace.categories must be a list of category names, got %T", v)
	}
	return nil
}

// categorySet is a set of enabled categories.
type categorySet uint8

func (s categorySet) has(c Category) bool {
	i := slices.Index(Categories, c)
	return i >= 0 && s&(1<<i) != 0
}

func parseCategories(names []string) (categorySet, error) {
	var s categorySet
	for _, n := range names {
		c := Category(strings.ToLower(strings.TrimSpace(n)))
		i := slices.Index(Categories, c)
		if i < 0 {
			valid := make([]string, len(Categories))
			for j, c := range Categories {
				valid[j] = string(c)
			}
			return 0, fmt.Errorf("correlator.trace.categories: unknown category %q (valid: %s)", n, strings.Join(valid, ", "))
		}
		s |= 1 << i
	}
	return s, nil
}
