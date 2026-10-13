package openmeteo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testDay = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

// fakeAPI is an in-memory Open-Meteo. For each requested coordinate it
// returns 24 hours starting at testDay, with every variable's value at hour
// h equal to h + 0.5, except for variables listed in nulls, which are null.
type fakeAPI struct {
	t     *testing.T
	nulls map[string]bool
	// status, if non-empty, is popped for each request and returned instead
	// of a normal response.
	status []int

	mu       sync.Mutex
	requests []*http.Request
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	var status int
	if len(f.status) > 0 {
		status, f.status = f.status[0], f.status[1:]
	}
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":true,"reason":"fake failure"}`))
		return
	}

	q := r.URL.Query()
	lats := strings.Split(q.Get("latitude"), ",")
	lons := strings.Split(q.Get("longitude"), ",")
	vars := strings.Split(q.Get("hourly"), ",")

	times := make([]string, 24)
	for h := range times {
		times[h] = testDay.Add(time.Duration(h) * time.Hour).Format(timeLayout)
	}

	locs := make([]map[string]any, len(lats))
	for i := range lats {
		lat, _ := strconv.ParseFloat(lats[i], 64)
		lon, _ := strconv.ParseFloat(lons[i], 64)
		hourly := map[string]any{"time": times}
		for _, v := range vars {
			vals := make([]*float64, 24)
			for h := range vals {
				if !f.nulls[v] {
					vals[h] = ptr(float64(h) + 0.5)
				}
			}
			hourly[v] = vals
		}
		locs[i] = map[string]any{
			"latitude":  lat + 0.01,
			"longitude": lon - 0.01,
			"elevation": 12.0,
			"hourly":    hourly,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if len(locs) == 1 {
		_ = json.NewEncoder(w).Encode(locs[0])
		return
	}
	_ = json.NewEncoder(w).Encode(locs)
}

func (f *fakeAPI) Requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func newTestClient(t *testing.T, api http.Handler, cfg Config) Client {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	cfg.URL = srv.URL
	cfg.BatchDelay = 0
	c, err := New(cfg, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	c.(*client).retryBackoff = time.Millisecond
	return c
}

func coords(n int) []Coordinate {
	out := make([]Coordinate, n)
	for i := range out {
		out[i] = Coordinate{Lat: 30 + float64(i)/100, Lon: -100 - float64(i)/100}
	}
	return out
}

func TestHourlyRequestShape(t *testing.T) {
	api := &fakeAPI{t: t}
	srv := httptest.NewServer(api)
	defer srv.Close()

	c, err := New(Config{URL: srv.URL, APIKey: "secret", Models: []string{"gfs_seamless", "icon_seamless"}, ForecastDays: 2})
	if err != nil {
		t.Fatal(err)
	}

	in := []Coordinate{{37.7749, -122.4194}, {46.8083, -100.7837}}
	if _, err := c.Hourly(context.Background(), in, []string{"temperature_2m", "dew_point_2m"}); err != nil {
		t.Fatal(err)
	}

	reqs := api.Requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	q := reqs[0].URL.Query()
	want := map[string]string{
		"latitude":      "37.7749,46.8083",
		"longitude":     "-122.4194,-100.7837",
		"hourly":        "temperature_2m,dew_point_2m",
		"timezone":      "GMT",
		"forecast_days": "2",
		"models":        "gfs_seamless,icon_seamless",
		"apikey":        "secret",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestHourlyNoOptionalParams(t *testing.T) {
	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{})
	if _, err := c.Hourly(context.Background(), coords(1), []string{"temperature_2m"}); err != nil {
		t.Fatal(err)
	}
	q := api.Requests()[0].URL.Query()
	for _, k := range []string{"models", "apikey"} {
		if q.Has(k) {
			t.Errorf("unexpected parameter %s=%q", k, q.Get(k))
		}
	}
}

func TestHourlyBatchingOrderAndDedupe(t *testing.T) {
	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{BatchSize: 50})

	in := coords(120)
	in = append(in, in[3], in[100]) // duplicates

	got, err := c.Hourly(context.Background(), in, []string{"temperature_2m"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(api.Requests()); n != 3 {
		t.Errorf("got %d requests, want 3", n)
	}
	if len(got) != 120 {
		t.Fatalf("got %d series, want 120", len(got))
	}
	for i, s := range got {
		if s.Requested != in[i] {
			t.Fatalf("series %d: requested %v, want %v", i, s.Requested, in[i])
		}
		if s.Grid.Lat != in[i].Lat+0.01 {
			t.Fatalf("series %d: grid not mapped from response", i)
		}
	}
}

func TestHourlySingleLocationResponse(t *testing.T) {
	c := newTestClient(t, &fakeAPI{t: t}, Config{})
	got, err := c.Hourly(context.Background(), coords(1), []string{"temperature_2m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1", len(got))
	}
	s := got[0]
	if s.Elevation != 12 || len(s.Times) != 24 || !s.Times[0].Equal(testDay) {
		t.Errorf("unexpected series: elevation=%v times=%d first=%v", s.Elevation, len(s.Times), s.Times[0])
	}
	if v := s.Values["temperature_2m"][5]; v == nil || *v != 5.5 {
		t.Errorf("temperature_2m[5] = %v, want 5.5", v)
	}
}

func TestProfiles(t *testing.T) {
	api := &fakeAPI{t: t, nulls: map[string]bool{"relative_humidity_925hPa": true}}
	c := newTestClient(t, api, Config{PressureLevels: []int{850, 1000, 925}})

	at := testDay.Add(6*time.Hour + 42*time.Minute) // within hour 6
	got, err := c.Profiles(context.Background(), coords(2), at)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d profiles, want 2", len(got))
	}

	vars := strings.Split(api.Requests()[0].URL.Query().Get("hourly"), ",")
	if len(vars) != 3+3*3 {
		t.Errorf("requested %d variables, want 12: %v", len(vars), vars)
	}

	p := got[0]
	if !p.Time.Equal(testDay.Add(6 * time.Hour)) {
		t.Errorf("time = %v, want hour 6", p.Time)
	}
	// Fake values at hour 6 are 6.5; temperatures are converted from °C.
	if got := *p.Surface.TemperatureK; got != 279.65 {
		t.Errorf("surface temperature = %v K, want 279.65", got)
	}
	if got := *p.Surface.PressureHPa; got != 6.5 {
		t.Errorf("surface pressure = %v, want 6.5", got)
	}
	if got := *p.Surface.HeightM; got != 14 {
		t.Errorf("surface height = %v, want elevation + 2 = 14", got)
	}

	wantLevels := []float64{1000, 925, 850}
	if len(p.Levels) != len(wantLevels) {
		t.Fatalf("got %d levels, want %d", len(p.Levels), len(wantLevels))
	}
	for i, l := range p.Levels {
		if *l.PressureHPa != wantLevels[i] {
			t.Errorf("level %d pressure = %v, want %v", i, *l.PressureHPa, wantLevels[i])
		}
		if *l.TemperatureK != 279.65 || *l.HeightM != 6.5 {
			t.Errorf("level %d: T=%v height=%v", i, *l.TemperatureK, *l.HeightM)
		}
	}
	if p.Levels[1].RelativeHumidityPct != nil {
		t.Errorf("925 hPa RH = %v, want nil", *p.Levels[1].RelativeHumidityPct)
	}
}

func TestProfilesHourNotFound(t *testing.T) {
	c := newTestClient(t, &fakeAPI{t: t}, Config{})
	got, err := c.Profiles(context.Background(), coords(2), testDay.Add(48*time.Hour))
	if !errors.Is(err, ErrHourNotFound) {
		t.Fatalf("err = %v, want ErrHourNotFound", err)
	}
	var ce *CoordinateError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a CoordinateError", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d profiles, want 0", len(got))
	}
}

func TestProfilesDenverFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/denver.json")
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}), Config{})

	got, err := c.Profiles(context.Background(), []Coordinate{{39.74, -104.99}}, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	p := got[0]
	if p.Elevation != 1597 {
		t.Errorf("elevation = %v, want 1597", p.Elevation)
	}
	if *p.Surface.PressureHPa != 843.1 || *p.Surface.TemperatureK != 301.15 {
		t.Errorf("surface = %v hPa, %v K; want 843.1 hPa, 301.15 K", *p.Surface.PressureHPa, *p.Surface.TemperatureK)
	}
	if len(p.Levels) != 8 {
		t.Fatalf("got %d levels, want 8", len(p.Levels))
	}

	// Levels below the terrain are passed through unchanged, not filtered.
	l1000 := p.Levels[0]
	if *l1000.PressureHPa != 1000 || *l1000.HeightM != 67 || *l1000.TemperatureK != 312.05 {
		t.Errorf("1000 hPa = %v m, %v K; want 67 m, 312.05 K", *l1000.HeightM, *l1000.TemperatureK)
	}
	var above int
	for _, l := range p.Levels {
		if *l.HeightM > p.Elevation {
			above++
		}
	}
	if above != 2 {
		t.Errorf("%d levels above ground, want 2 (800 and 700 hPa)", above)
	}
}

func TestAPIErrorNotRetried(t *testing.T) {
	api := &fakeAPI{t: t, status: []int{http.StatusBadRequest}}
	c := newTestClient(t, api, Config{MaxRetries: 2})

	_, err := c.Hourly(context.Background(), coords(1), []string{"bogus"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Reason != "fake failure" {
		t.Errorf("got %+v", apiErr)
	}
	var be *BatchError
	if !errors.As(err, &be) || len(be.Coordinates) != 1 {
		t.Errorf("err = %v, want BatchError for 1 coordinate", err)
	}
	if n := len(api.Requests()); n != 1 {
		t.Errorf("got %d requests, want 1 (no retry on 400)", n)
	}
}

func TestRetryOnServerError(t *testing.T) {
	api := &fakeAPI{t: t, status: []int{http.StatusInternalServerError, http.StatusTooManyRequests}}
	c := newTestClient(t, api, Config{MaxRetries: 2})

	got, err := c.Hourly(context.Background(), coords(1), []string{"temperature_2m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(api.Requests()) != 3 {
		t.Errorf("got %d series after %d requests, want 1 after 3", len(got), len(api.Requests()))
	}
}

func TestPartialBatchFailure(t *testing.T) {
	// First batch fails permanently, second succeeds.
	api := &fakeAPI{t: t, status: []int{http.StatusBadRequest}}
	c := newTestClient(t, api, Config{BatchSize: 2})

	got, err := c.Hourly(context.Background(), coords(4), []string{"temperature_2m"})
	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want BatchError", err)
	}
	if len(got) != 2 || got[0].Requested != coords(4)[2] {
		t.Errorf("got %d series, want the 2 from the second batch", len(got))
	}
}

func TestHostSelection(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantURL string
		wantErr bool
	}{
		{"free without key", Config{}, FreeURL, false},
		{"customer with key", Config{APIKey: "k"}, CustomerURL, false},
		{"explicit url wins", Config{APIKey: "k", URL: "http://meteo.local:8080/v1/forecast"}, "http://meteo.local:8080/v1/forecast", false},
		{"key with free host", Config{APIKey: "k", URL: FreeURL}, "", true},
		{"invalid url", Config{URL: "not a url"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := c.(*client).baseURL; got != tt.wantURL {
				t.Errorf("url = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.(*client).cfg
	if cfg.BatchSize != 50 || cfg.ForecastDays != 1 || cfg.Timeout != 10*time.Second || len(cfg.PressureLevels) != 8 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestAPIKeyRedacted(t *testing.T) {
	const key = "super-secret-key"
	c, err := New(Config{APIKey: key, URL: "http://127.0.0.1:1/v1/forecast", MaxRetries: 0})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Hourly(context.Background(), coords(1), []string{"temperature_2m"})
	if err == nil {
		t.Fatal("expected connection error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaks API key: %v", err)
	}
	if !strings.Contains(err.Error(), "apikey=REDACTED") {
		t.Errorf("error does not show redacted key: %v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	c := newTestClient(t, &fakeAPI{t: t}, Config{BatchSize: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Hourly(ctx, coords(3), []string{"temperature_2m"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestLive queries the real Open-Meteo API. Run with OPENMETEO_LIVE=1.
// Set OPENMETEO_API_KEY to use the customer endpoint.
func TestLive(t *testing.T) {
	if os.Getenv("OPENMETEO_LIVE") == "" {
		t.Skip("set OPENMETEO_LIVE=1 to query the real Open-Meteo API")
	}

	c, err := New(Config{APIKey: os.Getenv("OPENMETEO_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}

	in := []Coordinate{
		{37.7749, -122.4194}, // San Francisco LRF
		{38.1782, -121.9569}, // SF <-> Sacramento midpoint
		{38.5816, -121.4944}, // Sacramento transmitter
		{46.8083, -100.7837}, // Bismarck LRF
		{47.5192, -101.0368}, // BIS <-> Minot midpoint
		{48.2330, -101.2923}, // Minot transmitter
		{39.74, -104.99},     // Denver (high elevation)
	}
	got, err := c.Profiles(context.Background(), in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d profiles, want %d", len(got), len(in))
	}

	for _, p := range got {
		if len(p.Levels) != 8 {
			t.Errorf("%v: %d levels, want 8", p.Requested, len(p.Levels))
		}
		for _, l := range append([]Level{p.Surface}, p.Levels...) {
			if l.TemperatureK == nil || *l.TemperatureK < 180 || *l.TemperatureK > 330 {
				t.Errorf("%v: implausible temperature at %v hPa", p.Requested, l.PressureHPa)
			}
		}
		t.Logf("%v grid=%v elev=%.0fm surface=%.1fhPa/%.2fK", p.Requested, p.Grid, p.Elevation,
			*p.Surface.PressureHPa, *p.Surface.TemperatureK)
	}

	denver := got[len(got)-1]
	if denver.Elevation < 1500 || denver.Elevation > 1700 {
		t.Errorf("Denver elevation = %v, want about 1597", denver.Elevation)
	}
}
