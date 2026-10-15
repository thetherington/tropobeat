package mock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

var testNow = time.Date(2026, 10, 15, 14, 20, 0, 0, time.UTC)

// Test sites (LRF coordinates) at a range of latitudes.
var sites = map[string]openmeteo.Coordinate{
	"TERH":  {Lat: 39.465533, Lon: -87.4023442},
	"PROV":  {Lat: 41.800928, Lon: -71.343793},
	"BOS":   {Lat: 42.6525301, Lon: -71.219552},
	"MIA":   {Lat: 25.77, Lon: -80.19},
	"ANC":   {Lat: 61.2328041, Lon: -149.8735904},
	"SANJU": {Lat: 18.464969, Lon: -66.097109},
}

// analyse fetches profiles for coords from the mock through the real
// Open-Meteo client and runs the ducting calculation on each.
func analyse(t *testing.T, cfg Config, at time.Time, coords ...openmeteo.Coordinate) []ducting.Analysis {
	t.Helper()
	srv := httptest.NewServer(Handler(cfg, func() time.Time { return at }))
	defer srv.Close()

	client, err := openmeteo.New(openmeteo.Config{URL: srv.URL + "/v1/forecast", MaxRetries: 0})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := client.Profiles(context.Background(), coords, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != len(coords) {
		t.Fatalf("got %d profiles for %d coordinates", len(profiles), len(coords))
	}
	out := make([]ducting.Analysis, len(profiles))
	for i, p := range profiles {
		out[i] = ducting.Analyse(p, ducting.DefaultConfig)
	}
	return out
}

func traps(a ducting.Analysis, mhz float64) bool {
	for _, d := range a.Ducts {
		if d.Traps(mhz) {
			return true
		}
	}
	return false
}

func hasDuctType(a ducting.Analysis, typ ducting.DuctType) bool {
	for _, d := range a.Ducts {
		if d.Type == typ {
			return true
		}
	}
	return false
}

const (
	rf2MHz  = 57  // low VHF
	rf7MHz  = 177 // high VHF
	rf14MHz = 473 // UHF
)

func TestScenarioOutcomes(t *testing.T) {
	tests := []struct {
		scenario string
		check    func(t *testing.T, a ducting.Analysis)
	}{
		{ScenarioNormal, func(t *testing.T, a ducting.Analysis) {
			if a.Class != ducting.Normal || a.Score != 0 || len(a.Ducts) != 0 {
				t.Errorf("class %s score %.2f ducts %d; want normal, 0, none", a.Class, a.Score, len(a.Ducts))
			}
		}},
		{ScenarioSuperRefractive, func(t *testing.T, a ducting.Analysis) {
			if a.Class != ducting.SuperRefractive || a.Score <= 0 || a.Score >= 0.6 || len(a.Ducts) != 0 {
				t.Errorf("class %s score %.2f ducts %d; want super_refractive, 0–0.6, none", a.Class, a.Score, len(a.Ducts))
			}
		}},
		{ScenarioDucting, func(t *testing.T, a ducting.Analysis) {
			if a.Class != ducting.Ducting || len(a.Ducts) == 0 {
				t.Fatalf("class %s ducts %d; want a duct", a.Class, len(a.Ducts))
			}
			if traps(a, rf14MHz) || traps(a, 605) {
				t.Errorf("thin duct (%.0f m) should not trap any TV channel", a.Ducts[0].ThicknessM)
			}
		}},
		{ScenarioElevated, func(t *testing.T, a ducting.Analysis) {
			if a.Class != ducting.Ducting || !hasDuctType(a, ducting.ElevatedDuct) {
				t.Errorf("class %s ducts %+v; want an elevated duct", a.Class, a.Ducts)
			}
			if !traps(a, rf14MHz) {
				t.Errorf("elevated duct should trap UHF: %+v", a.Ducts)
			}
		}},
		{ScenarioTrappedUHF, func(t *testing.T, a ducting.Analysis) {
			if a.Class != ducting.Ducting || !hasDuctType(a, ducting.SurfaceDuct) {
				t.Fatalf("class %s ducts %+v; want a surface duct", a.Class, a.Ducts)
			}
			if !traps(a, rf14MHz) || traps(a, rf7MHz) {
				t.Errorf("duct %.0f m: traps UHF %v, VHF %v; want UHF only", a.Ducts[0].ThicknessM, traps(a, rf14MHz), traps(a, rf7MHz))
			}
		}},
		{ScenarioTrapped, func(t *testing.T, a ducting.Analysis) {
			if a.Class != ducting.Ducting || a.Score < 0.85 {
				t.Errorf("class %s score %.2f; want ducting, ≥0.85", a.Class, a.Score)
			}
			if !traps(a, rf2MHz) || !traps(a, rf7MHz) || !traps(a, rf14MHz) {
				t.Errorf("thick duct %+v should trap RF 2, 7 and 14", a.Ducts)
			}
		}},
	}

	for _, tt := range tests {
		for name, c := range sites {
			t.Run(tt.scenario+"/"+name, func(t *testing.T) {
				cfg := Config{Enabled: true, Scenario: tt.scenario, Cycle: time.Hour}
				tt.check(t, analyse(t, cfg, testNow, c)[0])
			})
		}
	}
}

func TestMixedEachSiteCyclesThroughEveryState(t *testing.T) {
	cfg := Config{Enabled: true, Scenario: ScenarioMixed, Cycle: 30 * time.Minute}
	for _, name := range []string{"TERH", "PROV", "BOS"} {
		t.Run(name, func(t *testing.T) {
			classes := map[ducting.Class]bool{}
			var uhf, vhf, ductNoTrap, elevated bool
			for m := 0; m < 30; m++ {
				a := analyse(t, cfg, testNow.Add(time.Duration(m)*time.Minute), sites[name])[0]
				classes[a.Class] = true
				uhf = uhf || traps(a, rf14MHz)
				vhf = vhf || traps(a, rf2MHz)
				elevated = elevated || hasDuctType(a, ducting.ElevatedDuct)
				ductNoTrap = ductNoTrap || (a.Class == ducting.Ducting && !traps(a, rf14MHz))
			}
			for _, c := range []ducting.Class{ducting.Normal, ducting.SuperRefractive, ducting.Ducting} {
				if !classes[c] {
					t.Errorf("class %s never seen in one cycle: %v", c, classes)
				}
			}
			if !uhf || !vhf || !ductNoTrap || !elevated {
				t.Errorf("in one cycle: traps UHF %v, traps RF 2 %v, untrapped duct %v, elevated %v; want all", uhf, vhf, ductNoTrap, elevated)
			}
		})
	}
}

func TestMixedSitesDiffer(t *testing.T) {
	cycle := 30 * time.Minute
	same := 0
	const samples = 30
	for m := 0; m < samples; m++ {
		at := testNow.Add(time.Duration(m) * time.Minute)
		var names []string
		for _, n := range []string{"TERH", "PROV", "BOS"} {
			s, _ := band(intensity(coordinate{sites[n].Lat, sites[n].Lon}, at, cycle))
			names = append(names, s)
		}
		if names[0] == names[1] && names[1] == names[2] {
			same++
		}
	}
	if same > samples/2 {
		t.Errorf("TERH, PROV and BOS in the same state at %d of %d instants; want them to differ most of the time", same, samples)
	}
}

func TestMixedNearbyPointsSimilar(t *testing.T) {
	cycle := 30 * time.Minute
	idx := map[string]int{}
	for i, b := range bands {
		idx[b.scenario] = i
	}
	for m := 0; m < 30; m++ {
		at := testNow.Add(time.Duration(m) * time.Minute)
		for _, c := range sites {
			a, _ := band(intensity(coordinate{c.Lat, c.Lon}, at, cycle))
			b, _ := band(intensity(coordinate{c.Lat + 0.05, c.Lon + 0.05}, at, cycle)) // ~5 mi away
			if d := idx[a] - idx[b]; d > 1 || d < -1 {
				t.Errorf("points 5 mi apart in non-adjacent states %s and %s", a, b)
			}
		}
	}
}

func TestMixedDeterministic(t *testing.T) {
	cfg := Config{Enabled: true, Scenario: ScenarioMixed, Cycle: time.Hour}
	a := analyse(t, cfg, testNow, sites["TERH"], sites["BOS"])
	b := analyse(t, cfg, testNow, sites["TERH"], sites["BOS"])
	for i := range a {
		if a[i].Score != b[i].Score || a[i].Class != b[i].Class {
			t.Errorf("same inputs gave different results: %v/%v vs %v/%v", a[i].Class, a[i].Score, b[i].Class, b[i].Score)
		}
	}
}

func TestResponseShape(t *testing.T) {
	srv := httptest.NewServer(Handler(DefaultConfig, func() time.Time { return testNow }))
	defer srv.Close()

	get := func(q string) (int, []byte) {
		res, err := http.Get(srv.URL + "/v1/forecast?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}

	// One location: a single object, 2 days of hours, requested variables only.
	code, body := get("latitude=39.47&longitude=-87.40&hourly=temperature_2m,geopotential_height_850hPa,boundary_layer_height&forecast_days=2")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var raw struct {
		Hourly map[string]json.RawMessage `json:"hourly"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	one := struct{ Hourly map[string][]*float64 }{Hourly: map[string][]*float64{}}
	for k, v := range raw.Hourly {
		if k == "time" {
			continue
		}
		var vals []*float64
		if err := json.Unmarshal(v, &vals); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		one.Hourly[k] = vals
	}
	var times struct {
		Hourly struct {
			Time []string `json:"time"`
		} `json:"hourly"`
	}
	_ = json.Unmarshal(body, &times)
	if len(times.Hourly.Time) != 48 || times.Hourly.Time[0] != "2026-10-15T00:00" {
		t.Errorf("times: %d starting %v; want 48 from 2026-10-15T00:00", len(times.Hourly.Time), times.Hourly.Time[:1])
	}
	if len(raw.Hourly) != 4 { // time + 3 variables
		t.Errorf("hourly keys: %d, want 4", len(raw.Hourly))
	}
	if one.Hourly["temperature_2m"][14] == nil || one.Hourly["geopotential_height_850hPa"][14] == nil {
		t.Error("simulated variables should have values")
	}
	if one.Hourly["boundary_layer_height"][14] != nil {
		t.Error("unsimulated variables should be null")
	}

	// Several locations: an array.
	code, body = get("latitude=39.47,42.65&longitude=-87.40,-71.22&hourly=temperature_2m")
	if code != 200 || !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		t.Errorf("multi-location response should be an array: %d %.60s", code, body)
	}

	// Bad input gives an Open-Meteo style error.
	code, body = get("latitude=39.47&longitude=&hourly=temperature_2m")
	if code != 400 || !strings.Contains(string(body), `"error":true`) {
		t.Errorf("bad request: %d %s", code, body)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig.Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
	if err := (Config{Scenario: "stormy", Cycle: time.Hour}).Validate(); err == nil {
		t.Error("unknown scenario should be rejected")
	}
	if err := (Config{Scenario: ScenarioMixed}).Validate(); err == nil {
		t.Error("zero cycle should be rejected")
	}
}
