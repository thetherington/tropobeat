package ducting

import (
	"math"

	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// Analysis is the ducting result for one location and hour.
type Analysis struct {
	// Points are the usable heights, lowest first.
	Points []Point
	// Layers are the gradients between consecutive points.
	Layers []Layer
	// Ducts are the ducts found, lowest first; empty when none.
	Ducts []Duct
	// Dropped is the number of pressure levels left out: below the terrain,
	// too close to the point below, or with missing values.
	Dropped int
	// Class is the class of the steepest layer, or InsufficientData.
	Class Class
	// MinDNdh is the most negative layer gradient dN/dh in N-units per km;
	// NaN when Class is InsufficientData.
	MinDNdh float64
	// Score is the 0–1 ducting likelihood; 0 when Class is InsufficientData.
	Score float64
}

// Usable reports whether the profile had enough heights to analyse.
func (a Analysis) Usable() bool { return a.Class != InsufficientData }

// Notable reports whether the location is super-refractive or ducting.
func (a Analysis) Notable() bool { return a.Class == SuperRefractive || a.Class == Ducting }

// Analyse computes the refractivity profile, layers, ducts, class and score
// of p.
func Analyse(p openmeteo.Profile, cfg Config) Analysis {
	pts, dropped := points(p, cfg.MinLayerM)
	a := Analysis{Points: pts, Dropped: dropped, Class: InsufficientData, MinDNdh: math.NaN()}
	if len(pts) < 2 {
		return a
	}
	a.Layers = layers(pts)
	a.Ducts = ducts(pts, a.Layers)
	a.MinDNdh = minGradient(a.Layers)
	a.Class = classify(a.MinDNdh)
	a.Score = Score(a.MinDNdh, cfg.Score)
	return a
}

// Score maps the steepest gradient dN/dh (N-units per km) to a 0–1 ducting
// likelihood, linearly within each band:
//
//	dN/dh ≥ −79           → 0                  (normal)
//	−79 > dN/dh ≥ −157    → 0 to 0.6           (super-refractive)
//	−157 > dN/dh ≥ −300   → 0.6 to 1           (ducting)
//	dN/dh < −300          → 1
//
// with the band edges and the 0.6 split taken from cfg.
func Score(dNdh float64, cfg ScoreConfig) float64 {
	sr, dt, st, ds := cfg.SuperRefractiveNPerKm, cfg.DuctingNPerKm, cfg.StrongNPerKm, cfg.DuctingScore
	switch {
	case math.IsNaN(dNdh) || dNdh >= sr:
		return 0
	case dNdh >= dt:
		return ds * (sr - dNdh) / (sr - dt)
	case dNdh >= st:
		return ds + (1-ds)*(dt-dNdh)/(dt-st)
	default:
		return 1
	}
}

// PathIndex is the plain mean of the scores of the usable analyses, and
// trapped reports whether any of their ducts traps mhz. ok is false when
// none is usable.
func PathIndex(as []Analysis, mhz float64) (index float64, trapped, ok bool) {
	n := 0
	for _, a := range as {
		if !a.Usable() {
			continue
		}
		n++
		index += a.Score
		for _, d := range a.Ducts {
			if d.Traps(mhz) {
				trapped = true
			}
		}
	}
	if n == 0 {
		return 0, false, false
	}
	return index / float64(n), trapped, true
}
