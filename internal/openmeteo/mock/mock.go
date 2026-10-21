// Package mock serves simulated weather in the Open-Meteo forecast format,
// for building and testing dashboards without calling the real API.
//
// Each location's weather is built so that tropobeat's ducting calculation
// produces a chosen outcome: normal air, super-refractive air, a thin duct
// that traps no TV channel, an elevated duct, a duct that traps UHF, or a
// thick duct that traps VHF too. The "mixed" scenario moves every location
// through all of these over a cycle, with nearby locations in similar
// states, so maps and dashboards show varied, changing data.
package mock

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Scenario names. Mixed varies with place and time; the others force one
// outcome everywhere.
const (
	ScenarioMixed           = "mixed"
	ScenarioNormal          = "normal"
	ScenarioSuperRefractive = "super_refractive"
	ScenarioDucting         = "ducting"
	ScenarioElevated        = "elevated"
	ScenarioTrappedUHF      = "trapped_uhf"
	ScenarioTrapped         = "trapped"
	// ScenarioNoData returns no values (nulls), so the ducting calculation
	// has insufficient data, as when the weather service has gaps.
	ScenarioNoData = "no_data"
)

// Scenarios lists every scenario name.
var Scenarios = []string{
	ScenarioMixed, ScenarioNormal, ScenarioSuperRefractive, ScenarioDucting,
	ScenarioElevated, ScenarioTrappedUHF, ScenarioTrapped, ScenarioNoData,
}

// Config configures the mock weather source.
type Config struct {
	// Enabled serves mock weather instead of calling Open-Meteo.
	Enabled bool `config:"enabled"`
	// Scenario is one of Scenarios.
	Scenario string `config:"scenario"`
	// Cycle is how long, with the mixed scenario, each location takes to
	// pass through every state.
	Cycle time.Duration `config:"cycle"`
	// Sites overrides the scenario for chosen LRFs (code → scenario): every
	// point of their co-channel paths gets that scenario, so a replayed
	// test can line up with known ducting. A point shared with another
	// LRF's path (a common transmitter) gets the override too.
	Sites map[string]string `config:"sites"`
	// FailEvery and FailFor make the mock answer HTTP 503 for FailFor at
	// the start of every FailEvery (aligned to the clock), so weather polls
	// fail and the ducting results go stale. 0 = never.
	FailEvery time.Duration `config:"fail_every"`
	FailFor   time.Duration `config:"fail_for"`
}

// DefaultConfig is disabled, with the mixed scenario on a 6-hour cycle.
var DefaultConfig = Config{
	Scenario: ScenarioMixed,
	Cycle:    6 * time.Hour,
}

// Validate checks the scenario and cycle.
func (c Config) Validate() error {
	if !slices.Contains(Scenarios, c.Scenario) {
		return fmt.Errorf("openmeteo.mock.scenario %q: must be one of %s", c.Scenario, strings.Join(Scenarios, ", "))
	}
	if c.Cycle <= 0 {
		return errors.New("openmeteo.mock.cycle must be positive")
	}
	for code, sc := range c.Sites {
		if !slices.Contains(Scenarios, sc) {
			return fmt.Errorf("openmeteo.mock.sites.%s %q: must be one of %s", code, sc, strings.Join(Scenarios, ", "))
		}
	}
	if c.FailEvery < 0 || c.FailFor < 0 || (c.FailEvery > 0 && c.FailFor >= c.FailEvery) {
		return errors.New("openmeteo.mock.fail_for must be shorter than fail_every, and neither negative")
	}
	return nil
}

// failing reports whether the mock answers 503 at t (FailEvery/FailFor).
func (c Config) failing(t time.Time) bool {
	if c.FailEvery <= 0 || c.FailFor <= 0 {
		return false
	}
	return time.Duration(t.UnixNano())%c.FailEvery < c.FailFor
}

// siteMatchDeg is how close (in degrees) a requested coordinate must be to
// a site override point to take its scenario. It covers the rounding of
// openmeteo.coordinate_precision.
const siteMatchDeg = 0.1

// SitePoint is a point of an overridden LRF's co-channel paths and the
// scenario it gets.
type SitePoint struct {
	Lat, Lon float64
	Scenario string
}

// Sites holds the site override points. Tropobeat replaces them after each
// co-channel discovery run; the handler reads them on every request.
type Sites struct {
	pts atomic.Pointer[[]SitePoint]
}

// Set replaces the override points.
func (s *Sites) Set(pts []SitePoint) { s.pts.Store(&pts) }

// scenario returns the scenario of the nearest override point within
// siteMatchDeg of c.
func (s *Sites) scenario(c coordinate) (string, bool) {
	if s == nil {
		return "", false
	}
	pts := s.pts.Load()
	if pts == nil {
		return "", false
	}
	best, bestD := "", math.Inf(1)
	for _, p := range *pts {
		dLat, dLon := math.Abs(p.Lat-c.lat), math.Abs(p.Lon-c.lon)
		if dLat > siteMatchDeg || dLon > siteMatchDeg {
			continue
		}
		if d := dLat*dLat + dLon*dLon; d < bestD {
			best, bestD = p.Scenario, d
		}
	}
	return best, best != ""
}

// timeLayout is Open-Meteo's hourly time format.
const timeLayout = "2006-01-02T15:04"

// Handler serves GET requests in the Open-Meteo forecast API format
// (latitude and longitude as comma-separated lists, hourly variables,
// forecast_days). now supplies the current time. sites holds the per-LRF
// scenario overrides (cfg.Sites); nil means none.
func Handler(cfg Config, now func() time.Time, sites *Sites) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.failing(now()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": true, "reason": "mock weather failure window (openmeteo.mock.fail_every)"})
			return
		}
		q := r.URL.Query()
		coords, err := parseCoordinates(q.Get("latitude"), q.Get("longitude"))
		if err != nil {
			writeError(w, err)
			return
		}
		vars := splitList(q.Get("hourly"))
		days := 1
		if v := q.Get("forecast_days"); v != "" {
			if days, err = strconv.Atoi(v); err != nil || days < 1 || days > 16 {
				writeError(w, fmt.Errorf("invalid forecast_days %q", v))
				return
			}
		}

		levels := pressureLevels(vars)
		t := now().UTC()
		dayStart := t.Truncate(24 * time.Hour)
		hourStart := t.Truncate(time.Hour)

		locs := make([]map[string]any, len(coords))
		for i, c := range coords {
			times := make([]string, days*24)
			hourly := map[string][]*float64{}
			for _, v := range vars {
				hourly[v] = make([]*float64, len(times))
			}
			for h := range times {
				at := dayStart.Add(time.Duration(h) * time.Hour)
				times[h] = at.Format(timeLayout)
				// The current hour uses the current time, so the mixed pattern
				// moves between polls; other hours keep the same offset.
				pc := cfg
				if sc, ok := sites.scenario(c); ok {
					pc.Scenario = sc
				}
				if pc.Scenario == ScenarioNoData {
					continue // every value stays null
				}
				p := build(pc, c, at.Add(t.Sub(hourStart)), levels)
				for _, v := range vars {
					hourly[v][h] = p.value(v)
				}
			}

			out := map[string]any{"time": times}
			for v, vals := range hourly {
				out[v] = vals
			}
			locs[i] = map[string]any{
				"latitude":              c.lat,
				"longitude":             c.lon,
				"elevation":             0.0,
				"generationtime_ms":     0.1,
				"utc_offset_seconds":    0,
				"timezone":              "GMT",
				"timezone_abbreviation": "GMT",
				"hourly":                out,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if len(locs) == 1 {
			_ = json.NewEncoder(w).Encode(locs[0])
			return
		}
		_ = json.NewEncoder(w).Encode(locs)
	})
}

type coordinate struct{ lat, lon float64 }

func parseCoordinates(lats, lons string) ([]coordinate, error) {
	la, lo := splitList(lats), splitList(lons)
	if len(la) == 0 || len(la) != len(lo) {
		return nil, fmt.Errorf("latitude and longitude must be non-empty lists of equal length")
	}
	out := make([]coordinate, len(la))
	for i := range la {
		lat, err1 := strconv.ParseFloat(la[i], 64)
		lon, err2 := strconv.ParseFloat(lo[i], 64)
		if err1 != nil || err2 != nil || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
			return nil, fmt.Errorf("invalid coordinate %q,%q", la[i], lo[i])
		}
		out[i] = coordinate{lat, lon}
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// pressureLevels returns the pressure levels (hPa) named in the requested
// variables, highest pressure (lowest height) first.
func pressureLevels(vars []string) []int {
	var out []int
	for _, v := range vars {
		i := strings.LastIndex(v, "_")
		if i < 0 || !strings.HasSuffix(v, "hPa") {
			continue
		}
		if l, err := strconv.Atoi(strings.TrimSuffix(v[i+1:], "hPa")); err == nil && l > 0 && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(a, b int) int { return b - a })
	return out
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": true, "reason": err.Error()})
}
