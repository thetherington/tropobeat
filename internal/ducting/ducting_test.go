package ducting

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/thetherington/tropobeat/internal/openmeteo"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f ± %v", name, got, want, tol)
	}
}

func ptr(v float64) *float64 { return &v }

func level(p, h, tK, rh float64) openmeteo.Level {
	return openmeteo.Level{PressureHPa: ptr(p), HeightM: ptr(h), TemperatureK: ptr(tK), RelativeHumidityPct: ptr(rh)}
}

// Hand-computed: 15 °C, 60 %, 1013.25 hPa.
func TestRefractivityWorkedExample(t *testing.T) {
	e := VapourPressure(288.15, 60)
	near(t, "e", e, 10.235, 0.005)
	n := Refractivity(1013.25, 288.15, e)
	near(t, "N", n, 318.88, 0.02)
	near(t, "M at 100 m", Modified(n, 100), n+15.7, 1e-9)
}

// dM/dh = dN/dh + 157 M/km, so dM/dh is zero exactly at dN/dh = −157.
func TestModifiedGradientZeroAtDuctingBoundary(t *testing.T) {
	n0 := 320.0
	n1 := n0 - 157*0.5 // −157 N/km over 500 m
	dMdh := (Modified(n1, 500) - Modified(n0, 0)) / 0.5
	near(t, "dM/dh", dMdh, 0, 1e-9)
}

func TestClassBoundaries(t *testing.T) {
	for _, tc := range []struct {
		dNdh float64
		want Class
	}{
		{10, SubRefractive},
		{0, Normal},
		{-40, Normal},
		{-79, Normal},
		{-79.1, SuperRefractive},
		{-157, SuperRefractive},
		{-157.1, Ducting},
		{-400, Ducting},
	} {
		if got := classify(tc.dNdh); got != tc.want {
			t.Errorf("classify(%v) = %s, want %s", tc.dNdh, got, tc.want)
		}
	}
}

func TestScoreBands(t *testing.T) {
	cfg := DefaultConfig.Score
	for _, tc := range []struct{ dNdh, want float64 }{
		{-40, 0},
		{-79, 0},
		{-118, 0.3},
		{-130, 0.6 * 51 / 78}, // the smoothing example: super-refractive
		{-157, 0.6},
		{-163.9, 0.6 + 0.4*6.9/143},
		{-300, 1},
		{-500, 1},
		{math.NaN(), 0},
	} {
		near(t, "Score", Score(tc.dNdh, cfg), tc.want, 1e-9)
	}
}

// The TERH example: a cool, humid surface under warm, dry air at 975 hPa.
// The 1000 hPa level is 18 m above the surface point, so it is dropped.
func terh() openmeteo.Profile {
	return openmeteo.Profile{
		Elevation: 150,
		Surface:   level(1002.1, 152, 284.2, 96),
		Levels: []openmeteo.Level{
			level(1000, 170, 284.4, 94),
			level(975, 380, 288.9, 41),
			level(950, 600, 287.5, 45),
		},
	}
}

func TestAnalyseTERH(t *testing.T) {
	a := Analyse(terh(), DefaultConfig)
	if a.Dropped != 1 || len(a.Points) != 3 || !a.Points[0].Surface {
		t.Fatalf("dropped %d, %d points (surface first: %v); want 1 dropped, 3 points", a.Dropped, len(a.Points), len(a.Points) > 0 && a.Points[0].Surface)
	}
	near(t, "surface N", a.Points[0].N, 332.07, 0.05)
	near(t, "975 hPa N", a.Points[1].N, 294.71, 0.05)
	near(t, "min dN/dh", a.MinDNdh, -163.9, 0.2)
	near(t, "first layer dM/dh", a.Layers[0].DMdh, a.Layers[0].DNdh+157, 1e-6)
	if a.Class != Ducting || a.Layers[1].Class != Normal {
		t.Errorf("class %s, layers %s/%s; want ducting, ducting/normal", a.Class, a.Layers[0].Class, a.Layers[1].Class)
	}
	near(t, "score", a.Score, 0.62, 0.005)

	if len(a.Ducts) != 1 {
		t.Fatalf("got %d ducts, want 1", len(a.Ducts))
	}
	d := a.Ducts[0]
	if d.Type != SurfaceDuct {
		t.Errorf("type = %s, want surface", d.Type)
	}
	near(t, "base", d.BaseAGLM, 2, 1e-9)
	near(t, "top", d.TopAGLM, 230, 1e-9)
	near(t, "thickness", d.ThicknessM, 228, 1e-9)
	near(t, "ΔM", d.DeltaM, 1.56, 0.05)
	near(t, "f_min", MinTrappedMHz(d.ThicknessM), 104.6, 0.1)
	if !d.Traps(195) || d.Traps(57) {
		t.Error("a 228 m duct should trap RF 10 (195 MHz) but not RF 2 (57 MHz)")
	}
}

func TestAnalyseStandardAtmosphere(t *testing.T) {
	// Temperature falling 6.5 K/km and moderate humidity: a normal gradient.
	p := openmeteo.Profile{
		Elevation: 0,
		Surface:   level(1013.25, 2, 288.15, 60),
		Levels: []openmeteo.Level{
			level(1000, 112, 287.4, 60),
			level(975, 323, 286.0, 58),
			level(950, 540, 284.6, 56),
			level(925, 762, 283.2, 54),
		},
	}
	a := Analyse(p, DefaultConfig)
	if a.Class != Normal || a.Score != 0 || len(a.Ducts) != 0 {
		t.Errorf("class %s, score %v, %d ducts; want normal, 0, none (min dN/dh %.1f)", a.Class, a.Score, len(a.Ducts), a.MinDNdh)
	}
}

func TestAnalyseInsufficientData(t *testing.T) {
	p := openmeteo.Profile{
		Elevation: 1597,
		Surface:   openmeteo.Level{}, // all missing
		Levels:    []openmeteo.Level{level(700, 3100, 270, 50)},
	}
	a := Analyse(p, DefaultConfig)
	if a.Usable() || a.Class != InsufficientData || a.Score != 0 || !math.IsNaN(a.MinDNdh) {
		t.Errorf("got class %s, score %v, min %v; want insufficient_data, 0, NaN", a.Class, a.Score, a.MinDNdh)
	}
}

// mkPoints builds points from heights (m, ground at 0) and M values.
func mkPoints(h, m []float64) []Point {
	pts := make([]Point, len(h))
	for i := range h {
		pts[i] = Point{HeightM: h[i], HeightAGLM: h[i], M: m[i], N: m[i] - mPerMetre*h[i], Surface: i == 0}
	}
	return pts
}

func TestDuctTypes(t *testing.T) {
	h := []float64{0, 200, 400, 600, 800}
	for _, tc := range []struct {
		name      string
		m         []float64
		want      DuctType
		base, dM  float64
		thickness float64
	}{
		// M falls from 360 to 340 between 400 and 600 m; below, M drops to
		// 340 between 200 m (330) and 400 m (360): base at 266.7 m.
		{"elevated", []float64{300, 330, 360, 340, 370}, ElevatedDuct, 200 + 200.0/3, 20, 600 - (200 + 200.0/3)},
		// M at every height below the trapping layer stays above 340.
		{"surface-based", []float64{350, 360, 370, 340, 380}, SurfaceBasedDuct, 0, 30, 600},
		{"surface", []float64{350, 340, 370, 400, 430}, SurfaceDuct, 0, 10, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pts := mkPoints(h, tc.m)
			ds := ducts(pts, layers(pts))
			if len(ds) != 1 {
				t.Fatalf("got %d ducts, want 1", len(ds))
			}
			d := ds[0]
			if d.Type != tc.want {
				t.Errorf("type = %s, want %s", d.Type, tc.want)
			}
			near(t, "base", d.BaseAGLM, tc.base, 1e-6)
			near(t, "ΔM", d.DeltaM, tc.dM, 1e-6)
			near(t, "thickness", d.ThicknessM, tc.thickness, 1e-6)
		})
	}
}

func TestTwoDuctsAndMultiLayerTrapping(t *testing.T) {
	// Trapping layers at 0–400 m (two layers) and 600–800 m.
	pts := mkPoints([]float64{0, 200, 400, 600, 800}, []float64{400, 390, 380, 420, 410})
	ds := ducts(pts, layers(pts))
	if len(ds) != 2 {
		t.Fatalf("got %d ducts, want 2", len(ds))
	}
	near(t, "first top", ds[0].TopAGLM, 400, 1e-9)
	near(t, "first ΔM", ds[0].DeltaM, 20, 1e-9)
	if ds[1].Type != ElevatedDuct {
		t.Errorf("second duct type = %s, want elevated", ds[1].Type)
	}
}

func TestChannelMHzAndTrapping(t *testing.T) {
	for _, tc := range []struct {
		rf   int
		mhz  float64
		minD float64 // minimum duct thickness, m
	}{
		{2, 57, 342},
		{7, 177, 161},
		{10, 195, 150},
		{14, 473, 83},
		{36, 605, 71},
	} {
		mhz, ok := ChannelMHz(tc.rf)
		if !ok || mhz != tc.mhz {
			t.Errorf("ChannelMHz(%d) = %v, %v; want %v", tc.rf, mhz, ok, tc.mhz)
		}
		// The thinnest duct that traps this channel.
		d := math.Pow(3.6e5/mhz, 2.0/3)
		near(t, "minimum thickness", d, tc.minD, 1)
	}
	if _, ok := ChannelMHz(1); ok {
		t.Error("RF 1 should not be a TV channel")
	}
	if !math.IsInf(MinTrappedMHz(0), 1) {
		t.Error("a zero-thickness duct traps nothing")
	}
}

func TestPathIndex(t *testing.T) {
	cfg := DefaultConfig
	lrf := Analyse(terh(), cfg)
	mid := Analysis{Class: SuperRefractive, Score: Score(-112.4, cfg.Score)}
	tx := Analysis{Class: Normal, Score: 0}
	near(t, "midpoint score", mid.Score, 0.257, 0.001)

	index, trapped, ok := PathIndex([]Analysis{lrf, mid, tx}, 195)
	if !ok || !trapped {
		t.Errorf("ok %v trapped %v, want both true", ok, trapped)
	}
	near(t, "index", index, (lrf.Score+mid.Score)/3, 1e-9)
	near(t, "index", index, 0.29, 0.005)

	// An unusable point is left out of the mean.
	index, _, _ = PathIndex([]Analysis{lrf, {Class: InsufficientData}}, 195)
	near(t, "index without the unusable point", index, lrf.Score, 1e-9)
	if _, _, ok := PathIndex([]Analysis{{Class: InsufficientData}}, 195); ok {
		t.Error("no usable point should give ok = false")
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := DefaultConfig
	bad.Publish = "some"
	if bad.Validate() == nil {
		t.Error("unknown publish mode should fail")
	}
	bad = DefaultConfig
	bad.Score.DuctingNPerKm = -50
	if bad.Validate() == nil {
		t.Error("out-of-order score bands should fail")
	}
}

// Denver: Open-Meteo extrapolates 1000–850 hPa below the 1597 m terrain.
func TestAnalyseDenverFixture(t *testing.T) {
	body, err := os.ReadFile("../openmeteo/testdata/denver.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	cfg := openmeteo.DefaultConfig
	cfg.URL = srv.URL
	c, err := openmeteo.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := c.Profiles(context.Background(), []openmeteo.Coordinate{{Lat: 39.74, Lon: -104.99}}, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	a := Analyse(ps[0], DefaultConfig)
	// Surface + 800 and 700 hPa; the six underground levels are dropped.
	if len(a.Points) != 3 || a.Dropped != 6 {
		t.Fatalf("%d points, %d dropped; want 3 and 6", len(a.Points), a.Dropped)
	}
	for _, p := range a.Points {
		if p.HeightAGLM < 2 {
			t.Errorf("point at %v m AGL is below the surface point", p.HeightAGLM)
		}
	}
	if !a.Usable() || len(a.Layers) != 2 {
		t.Errorf("class %s, %d layers; want usable with 2 layers", a.Class, len(a.Layers))
	}
}
