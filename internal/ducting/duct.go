package ducting

import "math"

// DuctType is where a duct sits relative to the ground (ITU-R P.453).
type DuctType string

const (
	// SurfaceDuct: the trapping layer starts at the lowest point (normally
	// the 2 m surface point).
	SurfaceDuct DuctType = "surface"
	// SurfaceBasedDuct: the trapping layer is elevated, but M at its top is
	// lower than anywhere below it, so the duct reaches down to the ground.
	SurfaceBasedDuct DuctType = "surface_based"
	// ElevatedDuct: the duct is clear of the ground. Its base is the height
	// below the trapping layer where M falls back to M at the layer's top.
	ElevatedDuct DuctType = "elevated"
)

// Duct is one duct found in a profile.
type Duct struct {
	Type DuctType
	// BaseAGLM and TopAGLM bound the duct, in metres above ground. The top is
	// the top of the trapping layer.
	BaseAGLM float64
	TopAGLM  float64
	// ThicknessM is TopAGLM − BaseAGLM.
	ThicknessM float64
	// DeltaM is the duct strength: M at the base of the trapping layer minus
	// M at its top, in M-units.
	DeltaM float64
}

// ducts finds the ducts in pts. A trapping layer is a run of consecutive
// layers whose class is Ducting (dM/dh < 0); each run gives one duct.
func ducts(pts []Point, ls []Layer) []Duct {
	var out []Duct
	for i := 0; i < len(ls); {
		if ls[i].Class != Ducting {
			i++
			continue
		}
		// The trapping layer runs from point i (its base) to point j (its
		// top); layer k lies between points k and k+1.
		j := i + 1
		for j < len(ls) && ls[j].Class == Ducting {
			j++
		}
		out = append(out, duct(pts, i, j))
		i = j
	}
	return out
}

// duct describes the duct whose trapping layer runs from pts[base] to
// pts[top].
func duct(pts []Point, base, top int) Duct {
	mTop := pts[top].M
	d := Duct{
		TopAGLM: pts[top].HeightAGLM,
		DeltaM:  pts[base].M - mTop,
	}
	switch {
	case base == 0:
		d.Type = SurfaceDuct
		d.BaseAGLM = pts[0].HeightAGLM
	default:
		// Below the trapping layer M normally falls going down. Walk down
		// until it drops to mTop; the duct's base is that height. If it never
		// does, the duct reaches the ground.
		d.Type = SurfaceBasedDuct
		d.BaseAGLM = pts[0].HeightAGLM
		for k := base - 1; k >= 0; k-- {
			lo, hi := pts[k], pts[k+1]
			if lo.M <= mTop {
				d.Type = ElevatedDuct
				// Interpolate linearly between lo and hi.
				f := (mTop - lo.M) / (hi.M - lo.M)
				d.BaseAGLM = lo.HeightAGLM + f*(hi.HeightAGLM-lo.HeightAGLM)
				break
			}
		}
	}
	d.ThicknessM = d.TopAGLM - d.BaseAGLM
	return d
}

// MinTrappedMHz returns the lowest frequency, in MHz, that a duct of the
// given thickness in metres can trap:
//
//	f_min = 3.6·10⁵ · d^(−3/2)
//
// From the US Naval Postgraduate School's EM propagation course (module 3.2,
// "Types of Ducts"). Lower frequencies leak out of the duct, which is why low
// VHF needs much thicker ducts than UHF.
func MinTrappedMHz(thicknessM float64) float64 {
	if thicknessM <= 0 {
		return math.Inf(1)
	}
	return 3.6e5 * math.Pow(thicknessM, -1.5)
}

// Traps reports whether d can trap a signal at mhz.
func (d Duct) Traps(mhz float64) bool {
	return mhz >= MinTrappedMHz(d.ThicknessM)
}

// ChannelMHz returns the centre frequency in MHz of a US TV RF channel (2–69),
// or false for any other channel.
func ChannelMHz(rf int) (float64, bool) {
	switch {
	case rf >= 2 && rf <= 4:
		return 57 + 6*float64(rf-2), true
	case rf >= 5 && rf <= 6:
		return 79 + 6*float64(rf-5), true
	case rf >= 7 && rf <= 13:
		return 177 + 6*float64(rf-7), true
	case rf >= 14 && rf <= 69:
		return 473 + 6*float64(rf-14), true
	default:
		return 0, false
	}
}
