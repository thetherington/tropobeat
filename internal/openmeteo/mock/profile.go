package mock

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/thetherington/tropobeat/internal/ducting"
)

// Standard-atmosphere constants for the hypsometric equation.
const (
	lapseKPerM   = 0.0065   // temperature lapse rate
	hypsoExp     = 0.190263 // R·Γ/g
	surfaceRHPct = 88.0     // humidity at the 2 m surface point
	surfaceHM    = 2.0      // the surface point's height (elevation is 0)
	minRHPct     = 2.0
	maxRHPct     = 100.0
)

// shape is the vertical structure a scenario asks for: the height of the
// 1000 hPa level (which sets the surface layer's thickness, and with it the
// thickness of a surface duct), and the target refractivity gradient dN/dh
// (N-units per km) of each layer from the bottom up. Layers beyond the list
// use the normal gradient.
type shape struct {
	gapM      float64
	gradients []float64
}

// normalNPerKm is the standard-atmosphere refractivity gradient.
const normalNPerKm = -40

// shapeFor returns the structure for a scenario. u (0–1) varies the strength
// within the scenario, so locations in the same state still differ.
func shapeFor(scenario string, u float64) shape {
	lerp := func(a, b float64) float64 { return a + (b-a)*u }
	switch scenario {
	case ScenarioSuperRefractive:
		// Steepest layer −90 to −150 N/km: super-refractive, score ≈ 0.1–0.55.
		return shape{gapM: 100, gradients: []float64{lerp(-90, -150)}}
	case ScenarioDucting:
		// A surface duct only ~40 m thick: too thin to trap any TV channel.
		return shape{gapM: 40, gradients: []float64{lerp(-200, -260)}}
	case ScenarioElevated:
		// Normal near the ground, a ducting layer between 975 and 950 hPa.
		return shape{gapM: 100, gradients: []float64{normalNPerKm, normalNPerKm, lerp(-220, -260)}}
	case ScenarioTrappedUHF:
		// A ~120 m surface duct: traps UHF (needs ~70–85 m) but not VHF.
		return shape{gapM: 120, gradients: []float64{lerp(-220, -260)}}
	case ScenarioTrapped:
		// A ~360 m surface duct across two layers: traps RF 2 and up.
		return shape{gapM: 150, gradients: []float64{lerp(-250, -280), lerp(-200, -230)}}
	default: // ScenarioNormal
		return shape{gapM: 100}
	}
}

// profile is the weather at one location and hour.
type profile struct {
	surfaceTempC    float64
	surfaceRH       float64
	surfacePressHPa float64
	levels          map[int]level // by pressure level, hPa
}

type level struct {
	heightM, tempC, rhPct float64
}

// value returns the Open-Meteo variable v, or nil if it isn't simulated.
func (p profile) value(v string) *float64 {
	f := func(x float64) *float64 { return &x }
	switch v {
	case "temperature_2m":
		return f(p.surfaceTempC)
	case "relative_humidity_2m":
		return f(p.surfaceRH)
	case "surface_pressure":
		return f(p.surfacePressHPa)
	}
	i := strings.LastIndex(v, "_")
	if i < 0 || !strings.HasSuffix(v, "hPa") {
		return nil
	}
	l, ok := p.levels[atoi(strings.TrimSuffix(v[i+1:], "hPa"))]
	if !ok {
		return nil
	}
	switch v[:i] {
	case "temperature":
		return f(l.tempC)
	case "relative_humidity":
		return f(l.rhPct)
	case "geopotential_height":
		return f(l.heightM)
	}
	return nil
}

// build returns the weather at c and time at for the configured scenario.
func build(cfg Config, c coordinate, at time.Time, levels []int) profile {
	scenario, u := cfg.Scenario, jitterUnit(c)
	if scenario == ScenarioMixed {
		scenario, u = band(intensity(c, at, cfg.Cycle))
	}
	return buildShape(c, shapeFor(scenario, u), levels)
}

// buildShape builds a profile whose refractivity gradients follow s. Each
// level's humidity is solved from its target refractivity, using the same
// formulas as the ducting calculation, and values are rounded the way
// Open-Meteo reports them.
func buildShape(c coordinate, s shape, levels []int) profile {
	// Warm enough everywhere to leave humidity headroom for strong ducts.
	t0 := math.Max(293, math.Min(301, 300-0.2*(math.Abs(c.lat)-25)))

	// Surface pressure that puts the 1000 hPa level s.gapM above sea level.
	p0 := round(1000/math.Pow(1-lapseKPerM*s.gapM/t0, 1/hypsoExp), 1)

	p := profile{
		surfaceTempC:    round(t0-273.15, 1),
		surfaceRH:       surfaceRHPct,
		surfacePressHPa: p0,
		levels:          map[int]level{},
	}
	tK := func(tC float64) float64 { return round(tC+273.15, 2) } // as the client converts

	prevH := surfaceHM
	prevN := ducting.Refractivity(p0, tK(p.surfaceTempC), ducting.VapourPressure(tK(p.surfaceTempC), surfaceRHPct))
	layer := 0
	for _, l := range levels {
		h := math.Round(t0 / lapseKPerM * (1 - math.Pow(float64(l)/p0, hypsoExp)))
		tC := round(t0-lapseKPerM*h-273.15, 1)
		if h <= prevH {
			// Below the surface (only for unusual pressure levels): not part
			// of the profile, so any plausible values will do.
			p.levels[l] = level{heightM: h, tempC: tC, rhPct: 50}
			continue
		}

		g := float64(normalNPerKm)
		if layer < len(s.gradients) {
			g = s.gradients[layer]
		}
		target := prevN + g*(h-prevH)/1000
		rh := round(solveRH(target, float64(l), tK(tC)), 1)
		p.levels[l] = level{heightM: h, tempC: tC, rhPct: rh}

		prevN = ducting.Refractivity(float64(l), tK(tC), ducting.VapourPressure(tK(tC), rh))
		prevH = h
		layer++
	}
	return p
}

// solveRH returns the relative humidity (%) that gives refractivity n at
// pressure pHPa and temperature tK, clamped to a plausible range.
func solveRH(n, pHPa, tK float64) float64 {
	// N = 77.6/T·(P + 4810·e/T)  →  e = (N·T/77.6 − P)·T/4810
	e := (n*tK/77.6 - pHPa) * tK / 4810
	es := ducting.VapourPressure(tK, 100)
	return math.Max(minRHPct, math.Min(maxRHPct, 100*e/es))
}

// Mixed pattern: a wave whose phase changes smoothly with position, so every
// location passes through every state over a cycle, nearby locations are in
// similar states, and distant ones differ.
const (
	milesPerDegree = 69.17
	waveMiles      = 1200 // one full phase turn along the SW→NE direction
	crossMiles     = 1500 // wavelength of the secondary component
	crossPhase     = 0.8  // amplitude (radians) of the secondary component
	jitterAmp      = 0.05
)

// intensity returns the mixed scenario's 0–1 ducting intensity at c and t.
func intensity(c coordinate, t time.Time, cycle time.Duration) float64 {
	x := c.lon * math.Cos(c.lat*math.Pi/180) * milesPerDegree
	y := c.lat * milesPerDegree
	along := (x + y) / math.Sqrt2
	across := (x - y) / math.Sqrt2
	phase := 2*math.Pi*along/waveMiles + crossPhase*math.Sin(2*math.Pi*across/crossMiles)

	tf := float64(t.UnixNano()) / float64(cycle)
	i := 0.5 + 0.5*math.Sin(2*math.Pi*tf+phase) + jitterAmp*(2*jitterUnit(c)-1)
	return math.Max(0, math.Min(1, i))
}

// Mixed-scenario bands: the upper bound of each state's intensity range.
var bands = []struct {
	upper    float64
	scenario string
}{
	{0.20, ScenarioNormal},
	{0.40, ScenarioSuperRefractive},
	{0.55, ScenarioDucting},
	{0.70, ScenarioElevated},
	{0.85, ScenarioTrappedUHF},
	{1.00, ScenarioTrapped},
}

// band maps an intensity to a scenario and the position (0–1) within its
// band.
func band(i float64) (string, float64) {
	lower := 0.0
	for _, b := range bands {
		if i < b.upper || b.upper == 1 {
			return b.scenario, math.Max(0, math.Min(1, (i-lower)/(b.upper-lower)))
		}
		lower = b.upper
	}
	return ScenarioTrapped, 1
}

// jitterUnit is a stable 0–1 value per ~0.1° cell.
func jitterUnit(c coordinate) float64 {
	h := fnv.New32a()
	fmt.Fprintf(h, "%.1f,%.1f", c.lat, c.lon)
	return float64(h.Sum32()%10000) / 9999
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
