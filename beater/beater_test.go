package beater

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/lrf"
	"github.com/thetherington/tropobeat/internal/openmeteo"
	"github.com/thetherington/tropobeat/internal/openmeteo/mock"
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
		"metricset.name":                         "cochannel",
		"tropobeat.discovery_run_id":             "20261013T030000Z",
		"tropobeat.cochannel.transmitter.erp_kw": 30.0,
		"tropobeat.cochannel.distance_mi":        99.5,
		"tropobeat.cochannel.path.type":          "LineString",
	}
	for k, want := range checks {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	checkLists(t, f, map[string]any{
		"tropobeat.lrf_codes":                []string{"TERH"},
		"tropobeat.path_ids":                 []string{"TERH:1001:10"},
		"tropobeat.rf_channels":              []int{10},
		"tropobeat.bands":                    []string{"high_vhf"},
		"tropobeat.transmitter_call_signs":   []string{"WFAR-TV"},
		"tropobeat.transmitter_facility_ids": []string{"1001"},
		"tropobeat.station_call_signs":       []string{"WTHI"},
	})
	// Discovery has no weather, so no forecast hour or ducting state.
	for _, k := range []string{"tropobeat.forecast_time", "tropobeat.ducting_state"} {
		if _, err := f.GetValue(k); err == nil {
			t.Errorf("%s set on a cochannel event", k)
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

	fetched := hour.Add(17 * time.Minute)
	ev := weatherEvent(g, &path, "TERH", "run1", fetched, nil)
	if !ev.Timestamp.Equal(fetched) {
		t.Errorf("timestamp = %v, want fetch time %v", ev.Timestamp, fetched)
	}
	f := ev.Fields
	if got := get(t, f, "tropobeat.forecast_time"); got != hour {
		t.Errorf("forecast_time = %v, want forecast hour %v", got, hour)
	}
	for k, want := range map[string]any{
		"metricset.name":                              "weather",
		"tropobeat.discovery_run_id":                  "run1",
		"tropobeat.weather.path.type":                 "LineString",
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
	checkLists(t, f, map[string]any{
		"tropobeat.lrf_codes":              []string{"TERH"},
		"tropobeat.path_ids":               []string{"TERH:1001:10"},
		"tropobeat.rf_channels":            []int{10},
		"tropobeat.transmitter_call_signs": []string{"WFAR-TV"},
	})

	// LRF-only group: only the LRF code, and no path fields. The group ID
	// (the LRF code) is not a path ID.
	lrfOnly := weatherEvent(openmeteo.GroupProfile{ID: "BOS", Points: g.Points[:1]}, nil, "BOS", "", fetched, nil).Fields
	checkLists(t, lrfOnly, map[string]any{"tropobeat.lrf_codes": []string{"BOS"}})
	for _, k := range []string{"tropobeat.path_ids", "tropobeat.rf_channels", "tropobeat.bands", "tropobeat.discovery_run_id", "tropobeat.weather.path"} {
		if _, err := lrfOnly.GetValue(k); err == nil {
			t.Errorf("LRF-only event should have no %s", k)
		}
	}
}

// checkLists compares list fields, which are published as typed slices.
func checkLists(t *testing.T, f mapstr.M, want map[string]any) {
	t.Helper()
	for k, w := range want {
		got := get(t, f, k)
		switch w := w.(type) {
		case []string:
			if g, ok := got.([]string); !ok || !slices.Equal(g, w) {
				t.Errorf("%s = %v, want %v", k, got, w)
			}
		case []int:
			if g, ok := got.([]int); !ok || !slices.Equal(g, w) {
				t.Errorf("%s = %v, want %v", k, got, w)
			}
		default:
			t.Fatalf("checkLists: unsupported type %T for %s", w, k)
		}
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
		{"weather on", mustClient(t), []string{"baseline", "cochannel", "weather"}},
		{"weather off", nil, []string{"baseline", "cochannel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newScheduler(logp.NewLogger("test"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Shutdown() }()

			bt := &tropobeat{ctx: context.Background(), logger: logp.NewLogger("test"), scheduler: s, weather: tc.weather}
			bt.config.Cochannel.Schedule = "0 3 * * *"
			bt.config.Baseline.Schedule = "30 3 * * *"
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
			bt.startAfterDiscoveryOnce()
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

func fp(v float64) *float64 { return &v }

func lvl(p, h, tK, rh float64) openmeteo.Level {
	return openmeteo.Level{PressureHPa: fp(p), HeightM: fp(h), TemperatureK: fp(tK), RelativeHumidityPct: fp(rh)}
}

// ductingFixture returns two paths sharing the TERH LRF, plus an LRF-only
// group (BOS). TERH has a surface duct; the midpoint of the first path is
// super-refractive; everything else is normal.
func ductingFixture() []fetchedGroup {
	hour := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	normal := func(lat, lon float64) *openmeteo.Profile {
		return &openmeteo.Profile{
			Requested: openmeteo.Coordinate{Lat: lat, Lon: lon}, Elevation: 0, Time: hour,
			Surface: lvl(1013.25, 2, 288.15, 60),
			Levels:  []openmeteo.Level{lvl(1000, 112, 287.4, 60), lvl(975, 323, 286.0, 58)},
		}
	}
	terh := &openmeteo.Profile{
		Requested: openmeteo.Coordinate{Lat: 39.465533, Lon: -87.4023442}, Elevation: 150, Time: hour,
		Surface: lvl(1002.1, 152, 284.2, 96),
		Levels:  []openmeteo.Level{lvl(1000, 170, 284.4, 94), lvl(975, 380, 288.9, 41), lvl(950, 600, 287.5, 45)},
	}
	// N falls about 100 N/km between the surface and 975 hPa.
	superMid := &openmeteo.Profile{
		Requested: openmeteo.Coordinate{Lat: 39.93, Lon: -86.7}, Elevation: 0, Time: hour,
		Surface: lvl(1013.25, 2, 288.15, 80),
		Levels:  []openmeteo.Level{lvl(975, 323, 288.5, 55)},
	}
	pp := func(label string, p *openmeteo.Profile) openmeteo.PointProfile {
		return openmeteo.PointProfile{Label: label, Coordinate: p.Requested, Profile: p}
	}

	a := testPath() // TERH:1001:10
	b := testPath()
	b.Transmitter.FacilityID = "2002"
	b.RFChannel = 2
	b.Midpoint = cochannel.Point{Lat: 38.5, Lon: -87.0}
	b.Transmitter.Location = cochannel.Point{Lat: 37.9, Lon: -86.6}

	return []fetchedGroup{
		{group: openmeteo.GroupProfile{ID: a.ID(), Points: []openmeteo.PointProfile{
			pp("lrf", terh), pp("midpoint", superMid), pp("transmitter", normal(40.4, -86.0)),
		}}, path: &a, lrfCode: "TERH"},
		{group: openmeteo.GroupProfile{ID: b.ID(), Points: []openmeteo.PointProfile{
			pp("lrf", terh), pp("midpoint", normal(38.5, -87.0)), pp("transmitter", normal(37.9, -86.6)),
		}}, path: &b, lrfCode: "TERH"},
		{group: openmeteo.GroupProfile{ID: "BOS", Points: []openmeteo.PointProfile{
			pp("lrf", normal(42.36, -71.06)),
		}}, lrfCode: "BOS"},
	}
}

func TestDuctingSummaryOnWeatherEvent(t *testing.T) {
	groups := ductingFixture()
	locs := analyseLocations(groups, ducting.DefaultConfig)
	at := time.Date(2026, 10, 14, 15, 17, 0, 0, time.UTC)

	f := weatherEvent(groups[0].group, groups[0].path, "TERH", "run1", at, locs).Fields
	for k, want := range map[string]any{
		"tropobeat.weather.lrf.ducting.profile_id":         "39.465533,-87.4023442",
		"tropobeat.weather.lrf.ducting.class":              "ducting",
		"tropobeat.weather.midpoint.ducting.class":         "super_refractive",
		"tropobeat.weather.transmitter.ducting.class":      "normal",
		"tropobeat.weather.transmitter.ducting.score":      0.0,
		"tropobeat.weather.ducting.frequency_mhz":          195.0,
		"tropobeat.weather.ducting.trapped":                true,
		"tropobeat.weather.midpoint.ducting.profile_id":    "39.93,-86.7",
		"tropobeat.weather.transmitter.ducting.profile_id": "40.4,-86",
		"tropobeat.ducting_state":                          "trapping",
		"tropobeat.ducting_level":                          3,
	} {
		if got := get(t, f, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	lrfScore := get(t, f, "tropobeat.weather.lrf.ducting.score").(float64)
	midScore := get(t, f, "tropobeat.weather.midpoint.ducting.score").(float64)
	index := get(t, f, "tropobeat.weather.ducting.index").(float64)
	if want := (lrfScore + midScore) / 3; index != want || midScore <= 0 || midScore >= 0.6 {
		t.Errorf("index = %v, want mean %v (lrf %v, midpoint %v)", index, want, lrfScore, midScore)
	}

	// RF 2 (57 MHz) needs a duct over 340 m thick; TERH's is 228 m.
	f = weatherEvent(groups[1].group, groups[1].path, "TERH", "run1", at, locs).Fields
	if got := get(t, f, "tropobeat.weather.ducting.trapped"); got != false {
		t.Errorf("RF 2 trapped = %v, want false", got)
	}
	if got := get(t, f, "tropobeat.ducting_state"); got != "ducting" {
		t.Errorf("RF 2 ducting_state = %v, want ducting", got)
	}

	// An LRF-only event gets the point summary and the state at the LRF, but
	// no path index.
	f = weatherEvent(groups[2].group, nil, "BOS", "run1", at, locs).Fields
	if _, err := f.GetValue("tropobeat.weather.ducting"); err == nil {
		t.Error("LRF-only event should have no path ducting index")
	}
	get(t, f, "tropobeat.weather.lrf.ducting.score")
	if got := get(t, f, "tropobeat.ducting_state"); got != "normal" {
		t.Errorf("BOS ducting_state = %v, want normal", got)
	}
}

func TestDuctingEvents(t *testing.T) {
	locs := analyseLocations(ductingFixture(), ducting.DefaultConfig)
	at := time.Date(2026, 10, 14, 15, 17, 0, 0, time.UTC)

	// 6 unique locations: TERH is shared by both paths.
	if len(locs) != 6 {
		t.Fatalf("%d locations, want 6", len(locs))
	}

	all, notable, insufficient := ductingEvents(locs, ducting.PublishAll, "run1", at)
	if len(all) != 6 || notable != 2 || insufficient != 0 {
		t.Errorf("all: %d events, %d notable, %d insufficient; want 6, 2, 0", len(all), notable, insufficient)
	}
	some, _, _ := ductingEvents(locs, ducting.PublishNotable, "run1", at)
	if len(some) != 2 {
		t.Errorf("notable: %d events, want 2", len(some))
	}

	var terh mapstr.M
	for _, ev := range all {
		if get(t, ev.Fields, "tropobeat.ducting.profile_id") == "39.465533,-87.4023442" {
			terh = ev.Fields
		}
	}
	if terh == nil {
		t.Fatal("no ducting event for TERH")
	}
	if !all[0].Timestamp.Equal(at) {
		t.Errorf("timestamp = %v, want %v", all[0].Timestamp, at)
	}
	for k, want := range map[string]any{
		"metricset.name":                   "ducting",
		"tropobeat.ducting.class":          "ducting",
		"tropobeat.ducting.dropped_levels": 1,
		"tropobeat.discovery_run_id":       "run1",
		"tropobeat.forecast_time":          time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC),
		"tropobeat.ducting.elevation_m":    150.0,
		// The duct traps RF 10 (one of the two paths), so the location is
		// trapping.
		"tropobeat.ducting_state": "trapping",
		"tropobeat.ducting_level": 3,
	} {
		if got := get(t, terh, k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	checkLists(t, terh, map[string]any{
		"tropobeat.path_ids":                 []string{"TERH:1001:10", "TERH:2002:2"},
		"tropobeat.lrf_codes":                []string{"TERH"},
		"tropobeat.rf_channels":              []int{2, 10},
		"tropobeat.bands":                    []string{"high_vhf", "low_vhf"},
		"tropobeat.transmitter_facility_ids": []string{"1001", "2002"},
		"tropobeat.transmitter_call_signs":   []string{"WFAR-TV"},
	})
	if n := len(get(t, terh, "tropobeat.ducting.points").([]mapstr.M)); n != 3 {
		t.Errorf("%d points, want 3", n)
	}
	ducts := get(t, terh, "tropobeat.ducting.ducts").([]mapstr.M)
	if len(ducts) != 1 || ducts[0]["type"] != "surface" || ducts[0]["thickness_m"] != 228.0 {
		t.Errorf("ducts = %v", ducts)
	}
}

func TestDuctingEventInsufficientData(t *testing.T) {
	p := &openmeteo.Profile{Requested: openmeteo.Coordinate{Lat: 39.74, Lon: -104.99}, Elevation: 1597}
	loc := &location{id: profileID(p), coord: p.Requested, profile: p, analysis: ducting.Analyse(*p, ducting.DefaultConfig)}
	f := ductingEvent(loc, "", time.Now()).Fields
	if get(t, f, "tropobeat.ducting.class") != "insufficient_data" {
		t.Error("want class insufficient_data")
	}
	if _, err := f.GetValue("tropobeat.ducting.score"); err == nil {
		t.Error("insufficient_data should have no score")
	}
}

func TestMockWeatherServer(t *testing.T) {
	cfg := mock.DefaultConfig
	cfg.Enabled = true
	srv, url, err := startMockWeather(cfg)
	if err != nil {
		t.Fatal(err)
	}

	client, err := openmeteo.New(openmeteo.Config{URL: url, MaxRetries: 0})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := client.Profiles(context.Background(), []openmeteo.Coordinate{{Lat: 39.47, Lon: -87.40}}, time.Now())
	if err != nil || len(profiles) != 1 || len(profiles[0].Levels) != 8 {
		t.Fatalf("profiles from mock: %d, err %v", len(profiles), err)
	}

	stopMockWeather(srv)
	if _, err := client.Profiles(context.Background(), []openmeteo.Coordinate{{Lat: 39.47, Lon: -87.40}}, time.Now()); err == nil {
		t.Error("requests should fail once the mock server is stopped")
	}
	stopMockWeather(nil) // safe without a server
}

func TestTagMock(t *testing.T) {
	events := []beat.Event{{Fields: mapstr.M{"metricset": mapstr.M{"name": "weather"}}}}
	tagMock(events)
	if tags, _ := events[0].Fields["tags"].([]string); len(tags) != 1 || tags[0] != "mock-weather" {
		t.Errorf("tags = %v", events[0].Fields["tags"])
	}
}
