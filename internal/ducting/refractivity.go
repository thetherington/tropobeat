// Package ducting turns an atmospheric profile into radio refractivity,
// layer gradients, ducts and a ducting likelihood score.
//
// The formulas follow ITU-R P.453 (The radio refractive index: its formula
// and refractivity data). Vertical resolution is limited to the weather
// model's pressure levels, so results are a likelihood rather than a
// measurement; see DUCTING.md in the project root.
package ducting

import "math"

// mPerMetre is the height term of modified refractivity: 10⁶ divided by
// the mean Earth radius (6,371 km), ≈ 0.157 M-units per metre.
const mPerMetre = 0.157

// VapourPressure returns the water vapour pressure e in hPa from the air
// temperature tK (Kelvin) and relative humidity rhPct (percent), using the
// Magnus–Tetens saturation vapour pressure over liquid water:
//
//	e = RH/100 · 6.11 · 10^(7.5·t / (t + 237.3)),  t = T − 273.15 °C
//
// Model humidity is relative to water at all temperatures, so the water
// form is used below 0 °C too.
func VapourPressure(tK, rhPct float64) float64 {
	t := tK - 273.15
	return rhPct / 100 * 6.11 * math.Pow(10, 7.5*t/(t+237.3))
}

// Refractivity returns the radio refractivity N (N-units) from the total
// air pressure pHPa (hPa), temperature tK (Kelvin) and water vapour
// pressure eHPa (hPa):
//
//	N = 77.6/T · (P + 4810·e/T)
//
// This is the form ITU-R P.453 gives for radio frequencies.
func Refractivity(pHPa, tK, eHPa float64) float64 {
	return 77.6 / tK * (pHPa + 4810*eHPa/tK)
}

// Modified returns the modified refractivity M (M-units) at height hM
// metres:
//
//	M = N + 0.157·h
//
// M takes the Earth's curvature into account, so a layer where M decreases
// with height (dM/dh < 0) bends radio waves at least as much as the Earth
// curves: a duct. Only differences in h matter for gradients, so h may be
// above sea level or above ground as long as it is used consistently.
func Modified(n, hM float64) float64 {
	return n + mPerMetre*hM
}
