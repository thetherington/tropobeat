package ducting

import "testing"

func TestStateOf(t *testing.T) {
	normal := Analysis{Class: Normal}
	sub := Analysis{Class: SubRefractive}
	super := Analysis{Class: SuperRefractive}
	// A 228 m duct traps down to about 105 MHz: RF 10 (195 MHz), not RF 2 (57 MHz).
	duct := Analysis{Class: Ducting, Ducts: []Duct{{Type: SurfaceDuct, ThicknessM: 228}}}
	none := Analysis{Class: InsufficientData}

	for _, tc := range []struct {
		name  string
		as    []Analysis
		mhz   []float64
		want  State
		level int
		ok    bool
	}{
		{"all normal", []Analysis{normal, sub, normal}, []float64{195}, StateNormal, 0, true},
		{"super-refractive midpoint", []Analysis{normal, super, normal}, []float64{195}, StateSuperRefractive, 1, true},
		{"duct too thin for RF 2", []Analysis{duct, super, normal}, []float64{57}, StateDucting, 2, true},
		{"duct traps RF 10", []Analysis{normal, super, duct}, []float64{195}, StateTrapping, 3, true},
		{"location used by RF 2 and RF 10 paths", []Analysis{duct}, []float64{57, 195}, StateTrapping, 3, true},
		{"no frequency never traps", []Analysis{duct}, nil, StateDucting, 2, true},
		{"unusable points are skipped", []Analysis{none, super}, []float64{195}, StateSuperRefractive, 1, true},
		{"nothing usable", []Analysis{none, none}, []float64{195}, StateNormal, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StateOf(tc.as, tc.mhz)
			if got != tc.want || ok != tc.ok || got.Level() != tc.level {
				t.Errorf("StateOf = %s (level %d), %v; want %s (level %d), %v", got, got.Level(), ok, tc.want, tc.level, tc.ok)
			}
		})
	}
}

func TestBand(t *testing.T) {
	for rf, want := range map[int]string{2: BandLowVHF, 6: BandLowVHF, 7: BandHighVHF, 13: BandHighVHF, 14: BandUHF, 36: BandUHF} {
		if got, ok := Band(rf); got != want || !ok {
			t.Errorf("Band(%d) = %q, %v; want %q", rf, got, ok, want)
		}
	}
	for _, rf := range []int{0, 1, 70} {
		if _, ok := Band(rf); ok {
			t.Errorf("Band(%d) ok, want not a TV channel", rf)
		}
	}
}
