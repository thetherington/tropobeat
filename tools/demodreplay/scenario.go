package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/thetherington/tropobeat/internal/demod"
)

// Scenario actions: how a step rewrites the reports it matches.
const (
	actionBER      = "ber"      // BER × factor (of the input's median, at least 1e-6)
	actionSaturate = "saturate" // BER 16.777215
	actionMER      = "mer"      // MER − db, power unchanged
	actionFade     = "fade"     // MER − db and power − db
	actionUnlock   = "unlock"   // Not Locked, MER 0, power −80
	actionDrop     = "drop"     // report withheld
	actionNoSide   = "noside"   // side label blanked
	actionFlap     = "flap"     // bad reports then good reports, repeating
)

var actions = []string{actionBER, actionSaturate, actionMER, actionFade, actionUnlock, actionDrop, actionNoSide, actionFlap}

// duration is a time.Duration written as "90s", "5m" or "1h30m" in YAML.
type duration time.Duration

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = duration(v)
	return nil
}

// Step is one timed injection.
type Step struct {
	// Name labels the step in the log.
	Name string `yaml:"name"`
	LRF  string `yaml:"lrf"`
	// RF is the RF channel, or "*" for every channel (with side A or B,
	// the whole frame).
	RF string `yaml:"rf"`
	// Side is A, B or AB (the default).
	Side string `yaml:"side"`
	// Start is the offset from the start of the scenario; Duration how
	// long the step lasts.
	Start    duration `yaml:"start"`
	Duration duration `yaml:"duration"`
	Action   string   `yaml:"action"`
	// Factor multiplies the BER for ber and flap (default 50).
	Factor float64 `yaml:"factor"`
	// DB is the MER (and, for fade, power) drop in dB (default 6).
	DB int `yaml:"db"`
	// Bad and Good are the report counts of each flap cycle (default 2, 2).
	Bad  int `yaml:"bad"`
	Good int `yaml:"good"`

	rf int // 0 = any channel
}

// Scenario is a list of steps that repeats every RepeatEvery.
type Scenario struct {
	// RepeatEvery restarts the scenario this long after it started. Unset:
	// the end of its last step plus 5 minutes. "0s": run once.
	RepeatEvery *duration `yaml:"repeat_every"`
	Steps       []Step    `yaml:"steps"`

	repeat time.Duration // 0 = run once
	lrfs   map[string]bool
}

// loadScenario reads and checks a scenario file.
func loadScenario(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseScenario(b)
}

func parseScenario(b []byte) (*Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("scenario: no steps")
	}
	s.lrfs = map[string]bool{}
	var end time.Duration
	for i := range s.Steps {
		st := &s.Steps[i]
		if st.Name == "" {
			st.Name = fmt.Sprintf("step %d", i+1)
		}
		st.LRF = demod.NormalizeLRF(st.LRF)
		if st.LRF == "" {
			return nil, fmt.Errorf("scenario %s: lrf is required", st.Name)
		}
		s.lrfs[st.LRF] = true
		switch strings.TrimSpace(st.RF) {
		case "", "*":
		default:
			n, err := strconv.Atoi(strings.TrimSpace(st.RF))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("scenario %s: rf must be a channel number or \"*\", got %q", st.Name, st.RF)
			}
			st.rf = n
		}
		st.Side = strings.ToUpper(strings.TrimSpace(st.Side))
		if st.Side == "" {
			st.Side = "AB"
		}
		if st.Side != "A" && st.Side != "B" && st.Side != "AB" {
			return nil, fmt.Errorf("scenario %s: side must be A, B or AB, got %q", st.Name, st.Side)
		}
		if !slices.Contains(actions, st.Action) {
			return nil, fmt.Errorf("scenario %s: action must be one of %s, got %q", st.Name, strings.Join(actions, ", "), st.Action)
		}
		if st.Duration <= 0 || st.Start < 0 {
			return nil, fmt.Errorf("scenario %s: duration must be positive and start not negative", st.Name)
		}
		if st.Factor <= 0 {
			st.Factor = 50
		}
		if st.DB <= 0 {
			st.DB = 6
		}
		if st.Bad <= 0 {
			st.Bad = 2
		}
		if st.Good <= 0 {
			st.Good = 2
		}
		end = max(end, time.Duration(st.Start+st.Duration))
	}
	switch {
	case s.RepeatEvery == nil:
		s.repeat = end + 5*time.Minute
	case *s.RepeatEvery > 0:
		s.repeat = time.Duration(*s.RepeatEvery)
		if s.repeat < end {
			return nil, fmt.Errorf("scenario: repeat_every %s is shorter than its last step's end (%s)", s.repeat, end)
		}
	}
	return &s, nil
}

// at returns the scenario offset and repetition number at replay offset t.
func (s *Scenario) at(t time.Duration) (offset time.Duration, rep int) {
	if s.repeat <= 0 {
		return t, 0
	}
	return t % s.repeat, int(t / s.repeat)
}

// active returns the indexes of the steps active at replay offset t, and
// the repetition number (only when the scenario repeats; otherwise steps
// are inactive after their end).
func (s *Scenario) active(t time.Duration) (idx []int, rep int) {
	off, rep := s.at(t)
	for i, st := range s.Steps {
		if off >= time.Duration(st.Start) && off < time.Duration(st.Start+st.Duration) {
			idx = append(idx, i)
		}
	}
	return idx, rep
}

// next returns how long after replay offset t the next step starts, or
// false when none will.
func (s *Scenario) next(t time.Duration) (time.Duration, bool) {
	off, _ := s.at(t)
	best, ok := time.Duration(0), false
	for _, st := range s.Steps {
		d := time.Duration(st.Start) - off
		if d <= 0 {
			if s.repeat <= 0 {
				continue
			}
			d += s.repeat
		}
		if !ok || d < best {
			best, ok = d, true
		}
	}
	return best, ok
}

func (st Step) matches(r *doc) bool {
	if r.lrf != st.LRF || (st.rf != 0 && r.rf != st.rf) {
		return false
	}
	return st.Side == "AB" || r.side == st.Side
}

// inputStats are an input's typical values in the loaded sample, used to
// size injections and to make -quiet readings healthy.
type inputStats struct {
	medianBER float64
	// bestMER is the highest locked MER in the sample. -quiet uses it rather
	// than the sample median: the sample covers minutes of one day, and its
	// median can sit at or below the threshold of the multi-day baseline.
	bestMER     int
	medianPower int
	lockedFrac  float64
}

// rewriter applies a scenario (and -quiet) to reports as they are replayed.
type rewriter struct {
	sc    *Scenario
	quiet bool
	stats map[string]inputStats
	// flapN counts each input's reports within a flap step, per step and
	// repetition, to alternate bad and good.
	flapN map[string]int
}

func newRewriter(sc *Scenario, quiet bool, stats map[string]inputStats) *rewriter {
	return &rewriter{sc: sc, quiet: quiet, stats: stats, flapN: map[string]int{}}
}

// apply rewrites r for replay offset t. It returns false when the report
// is to be withheld (a drop step).
func (w *rewriter) apply(r *doc, t time.Duration) bool {
	if w.sc == nil {
		return true
	}
	idx, rep := w.sc.active(t)
	var (
		step  *Step
		flapN int
	)
	for _, i := range idx {
		if w.sc.Steps[i].matches(r) {
			step = &w.sc.Steps[i]
			if step.Action == actionFlap {
				key := fmt.Sprintf("%d/%d/%s", i, rep, r.instance)
				w.flapN[key]++
				flapN = w.flapN[key]
			}
			break
		}
	}
	st := w.stats[r.instance]
	if w.quiet && w.sc.lrfs[r.lrf] && st.lockedFrac > 0.5 {
		// A normally locked input on a scenario LRF: healthy readings, so
		// only the injections show.
		r.set(fieldStatus, demod.StatusLocked)
		r.set(fieldBER, 0.0)
		r.set(fieldMER, st.bestMER)
		r.set(fieldPower, st.medianPower)
	}
	if step == nil {
		return true
	}
	ber := max(st.medianBER, 1e-6) * step.Factor
	switch step.Action {
	case actionDrop:
		return false
	case actionBER:
		r.set(fieldBER, ber)
	case actionSaturate:
		r.set(fieldBER, demod.BERSaturated)
	case actionMER:
		r.set(fieldMER, r.int(fieldMER)-step.DB)
	case actionFade:
		r.set(fieldMER, r.int(fieldMER)-step.DB)
		r.set(fieldPower, r.int(fieldPower)-step.DB)
	case actionUnlock:
		r.set(fieldStatus, "Not Locked")
		r.set(fieldMER, 0)
		r.set(fieldPower, -80)
	case actionNoSide:
		r.set(fieldSide, "")
	case actionFlap:
		if (flapN-1)%(step.Bad+step.Good) < step.Bad {
			r.set(fieldBER, ber)
		}
	}
	return true
}

// Demod report fields under rfdemod.rf_demod that the replay reads or
// rewrites.
const (
	fieldLRF    = "s_lrf_id"
	fieldSide   = "s_demod_side"
	fieldRF     = "i_rf_channel"
	fieldSlot   = "i_slot"
	fieldInput  = "i_input"
	fieldStatus = "s_input_status"
	fieldMER    = "i_input_mer"
	fieldPower  = "i_input_power"
	fieldBER    = "d_pre_fec_ber"
)

// doc is one exported report: its _source, and the fields used to match
// and size injections.
type doc struct {
	at     time.Time
	source map[string]any
	demod  map[string]any // source["rfdemod"]["rf_demod"]

	lrf, side, instance string
	rf                  int
}

func newDoc(at time.Time, raw map[string]json.RawMessage) (*doc, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	d := &doc{at: at}
	if err := json.Unmarshal(b, &d.source); err != nil {
		return nil, err
	}
	rd, _ := d.source["rfdemod"].(map[string]any)
	d.demod, _ = rd["rf_demod"].(map[string]any)
	if d.demod == nil {
		d.demod = map[string]any{}
	}
	d.lrf = demod.NormalizeLRF(d.str(fieldLRF))
	d.side = demod.NormalizeSide(d.str(fieldSide))
	d.rf = d.int(fieldRF)
	host, _ := d.source["host"].(map[string]any)
	ip, _ := host["ip"].(string)
	d.instance = demod.Instance{HostIP: ip, Slot: d.int(fieldSlot), Input: d.int(fieldInput)}.String()
	return d, nil
}

func (d *doc) str(f string) string {
	s, _ := d.demod[f].(string)
	return s
}

func (d *doc) int(f string) int {
	switch v := d.demod[f].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (d *doc) float(f string) float64 {
	switch v := d.demod[f].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// set changes a field of the report. Callers work on a copy (see clone).
func (d *doc) set(f string, v any) { d.demod[f] = v }

// clone copies the report so a rewrite doesn't change the loaded sample,
// which is replayed again on the next pass.
func (d *doc) clone() *doc {
	c := *d
	c.source = make(map[string]any, len(d.source))
	for k, v := range d.source {
		c.source[k] = v
	}
	rd := map[string]any{}
	if orig, ok := d.source["rfdemod"].(map[string]any); ok {
		for k, v := range orig {
			rd[k] = v
		}
	}
	c.demod = make(map[string]any, len(d.demod))
	for k, v := range d.demod {
		c.demod[k] = v
	}
	rd["rf_demod"] = c.demod
	c.source["rfdemod"] = rd
	return &c
}

// sampleStats works out each input's typical readings from its locked
// reports in the sample.
func sampleStats(docs []*doc) map[string]inputStats {
	type acc struct {
		ber          []float64
		mer, power   []int
		locked, seen int
	}
	accs := map[string]*acc{}
	for _, d := range docs {
		a := accs[d.instance]
		if a == nil {
			a = &acc{}
			accs[d.instance] = a
		}
		a.seen++
		if d.str(fieldStatus) != demod.StatusLocked {
			continue
		}
		a.locked++
		a.ber = append(a.ber, d.float(fieldBER))
		a.mer = append(a.mer, d.int(fieldMER))
		a.power = append(a.power, d.int(fieldPower))
	}
	out := make(map[string]inputStats, len(accs))
	for k, a := range accs {
		s := inputStats{lockedFrac: float64(a.locked) / float64(a.seen)}
		if a.locked > 0 {
			slices.Sort(a.ber)
			slices.Sort(a.mer)
			slices.Sort(a.power)
			s.medianBER, s.bestMER, s.medianPower = a.ber[len(a.ber)/2], a.mer[len(a.mer)-1], a.power[len(a.power)/2]
			if demod.IsSaturatedBER(s.medianBER) {
				s.medianBER = 0
			}
		}
		out[k] = s
	}
	return out
}
