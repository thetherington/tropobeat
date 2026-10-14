package ducting

import (
	"math"

	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// Class is the refraction class of a layer or a whole profile, from its
// refractivity gradient dN/dh (ITU-R P.453).
type Class string

const (
	// SubRefractive: dN/dh > 0. Signals bend less than normal and the radio
	// horizon shrinks.
	SubRefractive Class = "sub_refractive"
	// Normal: 0 ≥ dN/dh ≥ −79 N/km. The standard atmosphere is about −40.
	Normal Class = "normal"
	// SuperRefractive: −79 > dN/dh ≥ −157 N/km. Signals bend more than
	// normal and travel beyond the usual horizon.
	SuperRefractive Class = "super_refractive"
	// Ducting: dN/dh < −157 N/km, equivalently dM/dh < 0. Signals bend at
	// least as much as the Earth curves and can be trapped.
	Ducting Class = "ducting"
	// InsufficientData: fewer than two usable heights, so no gradient.
	InsufficientData Class = "insufficient_data"
)

// Class boundaries in N-units per km (ITU-R P.453).
const (
	superRefractiveNPerKm = -79
	ductingNPerKm         = -157
)

// classify returns the class of a gradient dN/dh in N-units per km.
func classify(dNdh float64) Class {
	switch {
	case dNdh > 0:
		return SubRefractive
	case dNdh >= superRefractiveNPerKm:
		return Normal
	case dNdh >= ductingNPerKm:
		return SuperRefractive
	default:
		return Ducting
	}
}

// Point is one height in a profile, with its refractivity.
type Point struct {
	PressureHPa         float64
	HeightM             float64 // above sea level
	HeightAGLM          float64 // above ground
	TemperatureK        float64
	RelativeHumidityPct float64
	VapourPressureHPa   float64
	N                   float64 // refractivity, N-units
	M                   float64 // modified refractivity, M-units
	// Surface is true for the 2 m surface point.
	Surface bool
}

// Layer is the air between two consecutive points.
type Layer struct {
	BaseAGLM float64
	TopAGLM  float64
	// DNdh and DMdh are the gradients in N- and M-units per km.
	DNdh  float64
	DMdh  float64
	Class Class
}

// points builds the usable points of p in increasing height: the surface
// point (if complete) and each pressure level with all values present that
// is at least minLayerM above the previous kept point. The first level must
// also be at least minLayerM above the surface height (Elevation + 2 m),
// even when the surface point itself is incomplete, so levels the model
// extrapolated below the terrain are dropped. dropped counts the levels
// left out.
func points(p openmeteo.Profile, minLayerM float64) (pts []Point, dropped int) {
	floor := p.Elevation + 2
	if pt, ok := point(p.Surface, p.Elevation); ok {
		pt.Surface = true
		pts = append(pts, pt)
		floor = pt.HeightM
	}
	for _, l := range p.Levels {
		pt, ok := point(l, p.Elevation)
		if !ok || pt.HeightM-floor < minLayerM {
			dropped++
			continue
		}
		pts = append(pts, pt)
		floor = pt.HeightM
	}
	return pts, dropped
}

// point converts a level, reporting false if any value is missing.
func point(l openmeteo.Level, elevation float64) (Point, bool) {
	if l.PressureHPa == nil || l.HeightM == nil || l.TemperatureK == nil || l.RelativeHumidityPct == nil {
		return Point{}, false
	}
	pt := Point{
		PressureHPa:         *l.PressureHPa,
		HeightM:             *l.HeightM,
		HeightAGLM:          *l.HeightM - elevation,
		TemperatureK:        *l.TemperatureK,
		RelativeHumidityPct: *l.RelativeHumidityPct,
	}
	pt.VapourPressureHPa = VapourPressure(pt.TemperatureK, pt.RelativeHumidityPct)
	pt.N = Refractivity(pt.PressureHPa, pt.TemperatureK, pt.VapourPressureHPa)
	pt.M = Modified(pt.N, pt.HeightM)
	return pt, true
}

// layers returns the gradients between consecutive points.
func layers(pts []Point) []Layer {
	var out []Layer
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		dzKm := (b.HeightM - a.HeightM) / 1000
		dNdh := (b.N - a.N) / dzKm
		out = append(out, Layer{
			BaseAGLM: a.HeightAGLM,
			TopAGLM:  b.HeightAGLM,
			DNdh:     dNdh,
			DMdh:     (b.M - a.M) / dzKm,
			Class:    classify(dNdh),
		})
	}
	return out
}

// minGradient returns the most negative dN/dh of ls, or NaN if there are no
// layers.
func minGradient(ls []Layer) float64 {
	lo := math.NaN()
	for _, l := range ls {
		if math.IsNaN(lo) || l.DNdh < lo {
			lo = l.DNdh
		}
	}
	return lo
}
