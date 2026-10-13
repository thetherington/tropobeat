package beater

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/lrf"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

func testPath() cochannel.Path {
	erp := 30.0
	return cochannel.Path{
		LRFCode:      "TERH",
		LRF:          cochannel.Point{Lat: 39.465533, Lon: -87.4023442},
		LRFCallSigns: []string{"WTHI"},
		RFChannel:    10,
		Transmitter: cochannel.Transmitter{
			CallSign: "WFAR-TV", FacilityID: "1001", Service: "DTV", ERPkW: &erp,
			Location: cochannel.Point{Lat: 40.4, Lon: -86.0},
		},
		Midpoint:   cochannel.Point{Lat: 39.93, Lon: -86.70},
		DistanceMi: 99.5,
		BearingDeg: 48.5,
	}
}

func get(t *testing.T, m mapstr.M, key string) any {
	t.Helper()
	v, err := m.GetValue(key)
	if err != nil {
		t.Fatalf("missing %s: %v", key, err)
	}
	return v
}

func TestCochannelEvent(t *testing.T) {
	at := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	ev := cochannelEvent(testPath(), "20261013T030000Z", at)

	if !ev.Timestamp.Equal(at) {
		t.Errorf("timestamp = %v", ev.Timestamp)
	}
	f := ev.Fields
	checks := map[string]any{
		"metricset.name":                            "cochannel",
		"tropobeat.cochannel.run_id":                "20261013T030000Z",
		"tropobeat.cochannel.path_id":               "TERH:1001:10",
		"tropobeat.cochannel.rf_channel":            10,
		"tropobeat.cochannel.lrf.code":              "TERH",
		"tropobeat.cochannel.transmitter.call_sign": "WFAR-TV",
		"tropobeat.cochannel.transmitter.erp_kw":    30.0,
		"tropobeat.cochannel.distance_mi":           99.5,
		"tropobeat.cochannel.path.type":             "LineString",
	}
	for k, want := range checks {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}

	// GeoJSON coordinates are [lon, lat]: LRF, midpoint, transmitter.
	coords := get(t, f, "tropobeat.cochannel.path.coordinates").([][]float64)
	want := [][]float64{{-87.4023442, 39.465533}, {-86.70, 39.93}, {-86.0, 40.4}}
	for i := range want {
		if coords[i][0] != want[i][0] || coords[i][1] != want[i][1] {
			t.Errorf("path point %d = %v, want %v", i, coords[i], want[i])
		}
	}
	if loc := get(t, f, "tropobeat.cochannel.midpoint.location").(mapstr.M); loc["lat"] != 39.93 || loc["lon"] != -86.70 {
		t.Errorf("midpoint location = %v", loc)
	}
}

func TestWeatherEvent(t *testing.T) {
	hour := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	tk, rh, p, h := 288.15, 55.0, 1000.0, 110.0
	prof := &openmeteo.Profile{
		Elevation: 150,
		Time:      hour,
		Surface:   openmeteo.Level{PressureHPa: &p, TemperatureK: &tk, RelativeHumidityPct: &rh},
		Levels:    []openmeteo.Level{{PressureHPa: &p, HeightM: &h, TemperatureK: &tk}},
	}
	path := testPath()
	g := openmeteo.GroupProfile{ID: path.ID(), Points: []openmeteo.PointProfile{
		{Label: "lrf", Coordinate: openmeteo.Coordinate{Lat: 39.465533, Lon: -87.4023442}, Profile: prof},
		{Label: "midpoint", Coordinate: openmeteo.Coordinate{Lat: 39.93, Lon: -86.70}, Profile: prof},
		{Label: "transmitter", Coordinate: openmeteo.Coordinate{Lat: 40.4, Lon: -86.0}, Profile: prof},
	}}

	ev := weatherEvent(g, &path, "TERH", "run1")
	if !ev.Timestamp.Equal(hour) {
		t.Errorf("timestamp = %v, want forecast hour", ev.Timestamp)
	}
	f := ev.Fields
	for k, want := range map[string]any{
		"metricset.name":                              "weather",
		"tropobeat.weather.path_id":                   "TERH:1001:10",
		"tropobeat.weather.lrf_code":                  "TERH",
		"tropobeat.weather.rf_channel":                10,
		"tropobeat.weather.transmitter_call_sign":     "WFAR-TV",
		"tropobeat.weather.midpoint.elevation_m":      150.0,
		"tropobeat.weather.lrf.surface.temperature_k": 288.15,
	} {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	levels := get(t, f, "tropobeat.weather.transmitter.levels").([]mapstr.M)
	if len(levels) != 1 || levels[0]["height_m"] != 110.0 {
		t.Errorf("levels = %v", levels)
	}
	if _, ok := levels[0]["relative_humidity_pct"]; ok {
		t.Error("nil values should be omitted")
	}

	// LRF-only group: no path fields.
	lrfOnly := weatherEvent(openmeteo.GroupProfile{ID: "BOS", Points: g.Points[:1]}, nil, "BOS", "")
	if _, err := lrfOnly.Fields.GetValue("tropobeat.weather.rf_channel"); err == nil {
		t.Error("LRF-only event should have no rf_channel")
	}
}

func TestFilterReceived(t *testing.T) {
	all := []lrf.LRF{{Code: "TERH"}, {Code: "PROV"}, {Code: "BOS"}}
	received := []cochannel.Received{{LRFCode: "TERH"}, {LRFCode: "PROV"}, {LRFCode: "BIRM"}}

	codes := func(rs []cochannel.Received) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.LRFCode)
		}
		return out
	}

	// Not limited: everything kept, so unknown IDs (BIRM) get reported.
	if got := codes(filterReceived(received, all, all, false)); len(got) != 3 {
		t.Errorf("unlimited: %v", got)
	}
	// Limited to TERH: PROV (filtered out) and BIRM (noise) dropped.
	if got := codes(filterReceived(received, all, all[:1], true)); len(got) != 1 || got[0] != "TERH" {
		t.Errorf("limited: %v, want [TERH]", got)
	}
}

func TestGocronLogger(t *testing.T) {
	// The adapter must accept gocron's key/value args without panicking.
	l := gocronLogger{logp.NewLogger("test")}
	l.Debug("debug", "job", "weather")
	l.Info("info", "job", "cochannel", "id", 1)
	l.Warn("warn")
	l.Error("error", "err", "boom")
}

func TestRegisterJobsWeatherDisabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		weather openmeteo.Client
		want    []string
	}{
		{"weather on", mustClient(t), []string{"cochannel", "weather"}},
		{"weather off", nil, []string{"cochannel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newScheduler(logp.NewLogger("test"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Shutdown() }()

			bt := &tropobeat{ctx: context.Background(), logger: logp.NewLogger("test"), scheduler: s, weather: tc.weather}
			bt.config.Cochannel.Schedule = "0 3 * * *"
			bt.config.Period = time.Hour
			if err := bt.registerJobs(); err != nil {
				t.Fatal(err)
			}

			var names []string
			for _, j := range s.Jobs() {
				names = append(names, j.Name())
			}
			slices.Sort(names)
			if !slices.Equal(names, tc.want) {
				t.Errorf("jobs = %v, want %v", names, tc.want)
			}
			// The first-poll trigger must be safe without a weather job.
			bt.startWeatherOnce()
		})
	}
}

func mustClient(t *testing.T) openmeteo.Client {
	t.Helper()
	c, err := openmeteo.New(openmeteo.Config{URL: "http://127.0.0.1:9/v1/forecast"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
