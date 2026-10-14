package ducting

import (
	"fmt"
	"math"
	"testing"
)

// ituRefractivity is the full ITU-R P.453-14 refractivity: the saturation
// vapour pressure over water with its enhancement factor, and the
// three-term refractivity with dry pressure Pd = P − e. It exists only to
// check the simpler formulas used by the package.
func ituRefractivity(pHPa, tK, rhPct float64) float64 {
	t := tK - 273.15
	ef := 1 + 1e-4*(7.2+pHPa*(0.0320+5.9e-6*t*t))
	es := ef * 6.1121 * math.Exp((18.678-t/234.5)*t/(t+257.14))
	e := rhPct / 100 * es
	return 77.6*(pHPa-e)/tK + 72*e/tK + 3.75e5*e/(tK*tK)
}

// The package formulas stay within 1.5 N-units of the full ITU-R P.453-14
// forms over the range met in the lowest 3 km. Duct gradients are at least
// 16 N-units per 100 m, so this difference doesn't change a class.
func TestFormulasAgainstITU(t *testing.T) {
	worst, at := 0.0, ""
	for tC := -40.0; tC <= 35; tC += 5 {
		for rh := 0.0; rh <= 100; rh += 10 {
			for p := 700.0; p <= 1050; p += 50 {
				tK := tC + 273.15
				ours := Refractivity(p, tK, VapourPressure(tK, rh))
				d := math.Abs(ours - ituRefractivity(p, tK, rh))
				if d > worst {
					worst = d
					at = fmt.Sprintf("%v °C, %v %%, %v hPa", tC, rh, p)
				}
			}
		}
	}
	t.Logf("largest difference %.2f N-units at %s", worst, at)
	if worst > 1.5 {
		t.Errorf("largest difference %.2f N-units at %s, want ≤ 1.5", worst, at)
	}
}
