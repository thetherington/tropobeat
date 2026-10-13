package openmeteo

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	lrfABL = Coordinate{Lat: 32.4449556, Lon: -99.7141538}
	txA    = Coordinate{Lat: 33.5, Lon: -101.9}
	txB    = Coordinate{Lat: 31.1, Lon: -97.3}
	midA   = Coordinate{Lat: 32.97, Lon: -100.8}
	midB   = Coordinate{Lat: 31.77, Lon: -98.5}
)

func pathGroup(id string, lrf, mid, tx Coordinate) Group {
	return Group{ID: id, Points: []Point{
		{Label: "lrf", Coordinate: lrf},
		{Label: "midpoint", Coordinate: mid},
		{Label: "transmitter", Coordinate: tx},
	}}
}

func TestGroupProfilesRegroupsAndDedupes(t *testing.T) {
	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{BatchSize: 50})

	groups := []Group{
		pathGroup("ABL:A", lrfABL, midA, txA),
		pathGroup("ABL:B", lrfABL, midB, txB), // shares the LRF with ABL:A
	}

	got, err := c.GroupProfiles(context.Background(), groups, testDay.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 6 points, 5 unique coordinates, one request.
	reqs := api.Requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	if n := len(strings.Split(reqs[0].URL.Query().Get("latitude"), ",")); n != 5 {
		t.Errorf("requested %d coordinates, want 5 unique", n)
	}

	if len(got) != len(groups) {
		t.Fatalf("got %d groups, want %d", len(got), len(groups))
	}
	for i, g := range got {
		if g.ID != groups[i].ID {
			t.Errorf("group %d: ID %q, want %q", i, g.ID, groups[i].ID)
		}
		if !g.Complete() {
			t.Errorf("group %s incomplete", g.ID)
		}
		for j, p := range g.Points {
			want := groups[i].Points[j]
			if p.Label != want.Label || p.Coordinate != want.Coordinate {
				t.Errorf("group %s point %d: got %s %v, want %s %v", g.ID, j, p.Label, p.Coordinate, want.Label, want.Coordinate)
			}
			if p.Profile.Requested != want.Coordinate {
				t.Errorf("group %s point %s: profile for %v, want %v", g.ID, p.Label, p.Profile.Requested, want.Coordinate)
			}
			if !p.Profile.Time.Equal(testDay.Add(3 * time.Hour)) {
				t.Errorf("group %s point %s: time %v", g.ID, p.Label, p.Profile.Time)
			}
		}
	}

	// Shared points refer to the same fetched profile.
	if got[0].Points[0].Profile != got[1].Points[0].Profile {
		t.Error("shared LRF should map to the same profile in both groups")
	}
}

func TestGroupProfilesBatchesAcrossGroups(t *testing.T) {
	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{BatchSize: 2})

	groups := []Group{
		pathGroup("ABL:A", lrfABL, midA, txA),
		pathGroup("ABL:B", lrfABL, midB, txB),
	}
	got, err := c.GroupProfiles(context.Background(), groups, testDay)
	if err != nil {
		t.Fatal(err)
	}
	// 5 unique coordinates in batches of 2 = 3 requests; batches span groups.
	if n := len(api.Requests()); n != 3 {
		t.Errorf("got %d requests, want 3", n)
	}
	for _, g := range got {
		if !g.Complete() {
			t.Errorf("group %s incomplete", g.ID)
		}
	}
}

func TestGroupProfilesPartialFailure(t *testing.T) {
	// The first batch (LRF + midpoint A) fails; the rest succeed.
	api := &fakeAPI{t: t, status: []int{http.StatusBadRequest}}
	c := newTestClient(t, api, Config{BatchSize: 2})

	groups := []Group{
		pathGroup("ABL:A", lrfABL, midA, txA),
		pathGroup("ABL:B", lrfABL, midB, txB),
	}
	got, err := c.GroupProfiles(context.Background(), groups, testDay)

	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want BatchError", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2 even on partial failure", len(got))
	}

	a := got[0]
	if a.Complete() {
		t.Error("ABL:A should be incomplete")
	}
	if a.Points[0].Profile != nil || a.Points[1].Profile != nil {
		t.Error("LRF and midpoint A were in the failed batch and should have nil profiles")
	}
	if a.Points[2].Profile == nil {
		t.Error("transmitter A should have a profile")
	}
	if b := got[1]; b.Points[0].Profile != nil || b.Points[1].Profile == nil || b.Points[2].Profile == nil {
		t.Error("ABL:B should be missing only the shared LRF")
	}
}

func TestGroupProfilesEmpty(t *testing.T) {
	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{})

	got, err := c.GroupProfiles(context.Background(), []Group{{ID: "empty"}}, testDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "empty" || len(got[0].Points) != 0 || !got[0].Complete() {
		t.Errorf("got %+v, want one empty group", got)
	}
	if n := len(api.Requests()); n != 0 {
		t.Errorf("got %d requests, want 0", n)
	}
}

// TestLiveGroups queries the real Open-Meteo API with the path groups from
// scripts/Meteo_Script.py. Run with OPENMETEO_LIVE=1.
func TestLiveGroups(t *testing.T) {
	if os.Getenv("OPENMETEO_LIVE") == "" {
		t.Skip("set OPENMETEO_LIVE=1 to query the real Open-Meteo API")
	}

	c, err := New(Config{APIKey: os.Getenv("OPENMETEO_API_KEY")})
	if err != nil {
		t.Fatal(err)
	}

	groups := []Group{
		pathGroup("SF:SAC", Coordinate{37.7749, -122.4194}, Coordinate{38.1782, -121.9569}, Coordinate{38.5816, -121.4944}),
		pathGroup("BIS:MOT", Coordinate{46.8083, -100.7837}, Coordinate{47.5192, -101.0368}, Coordinate{48.2330, -101.2923}),
	}
	got, err := c.GroupProfiles(context.Background(), groups, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if !g.Complete() {
			t.Errorf("group %s incomplete", g.ID)
			continue
		}
		for _, p := range g.Points {
			t.Logf("%-8s %-11s %v elev=%.0fm surface=%.2fK", g.ID, p.Label, p.Coordinate, p.Profile.Elevation, *p.Profile.Surface.TemperatureK)
		}
	}
}

func TestGroupProfilesCoordinatePrecisionMerges(t *testing.T) {
	// Two midpoints about 300 m apart fall in the same 0.01° cell.
	near1 := Coordinate{Lat: 41.7031, Lon: -72.8312}
	near2 := Coordinate{Lat: 41.7044, Lon: -72.8329}
	groups := []Group{
		{ID: "a", Points: []Point{{Label: "midpoint", Coordinate: near1}}},
		{ID: "b", Points: []Point{{Label: "midpoint", Coordinate: near2}}},
	}

	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{CoordinatePrecision: 0.01})
	got, err := c.GroupProfiles(context.Background(), groups, testDay)
	if err != nil {
		t.Fatal(err)
	}

	q := api.Requests()[0].URL.Query()
	if q.Get("latitude") != "41.7" || q.Get("longitude") != "-72.83" {
		t.Errorf("requested %s,%s; want one merged location 41.7,-72.83", q.Get("latitude"), q.Get("longitude"))
	}
	for i, g := range got {
		p := g.Points[0]
		if p.Profile == nil {
			t.Fatalf("group %s has no profile", g.ID)
		}
		if p.Coordinate != groups[i].Points[0].Coordinate {
			t.Errorf("group %s: coordinate %v, want original %v", g.ID, p.Coordinate, groups[i].Points[0].Coordinate)
		}
	}
	if got[0].Points[0].Profile != got[1].Points[0].Profile {
		t.Error("merged points should share one profile")
	}
}

func TestGroupProfilesNoPrecisionKeepsBoth(t *testing.T) {
	api := &fakeAPI{t: t}
	c := newTestClient(t, api, Config{})
	groups := []Group{
		{ID: "a", Points: []Point{{Label: "midpoint", Coordinate: Coordinate{Lat: 41.7031, Lon: -72.8312}}}},
		{ID: "b", Points: []Point{{Label: "midpoint", Coordinate: Coordinate{Lat: 41.7044, Lon: -72.8329}}}},
	}
	if _, err := c.GroupProfiles(context.Background(), groups, testDay); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(api.Requests()[0].URL.Query().Get("latitude"), ",")); n != 2 {
		t.Errorf("requested %d locations, want 2", n)
	}
}

func TestRound(t *testing.T) {
	tests := []struct {
		in        Coordinate
		precision float64
		want      Coordinate
	}{
		{Coordinate{41.7031, -72.8312}, 0, Coordinate{41.7031, -72.8312}},
		{Coordinate{41.7031, -72.8312}, 0.01, Coordinate{41.7, -72.83}},
		{Coordinate{-33.8675, 151.2069}, 0.05, Coordinate{-33.85, 151.2}},
		{Coordinate{0.3, -0.3}, 0.1, Coordinate{0.3, -0.3}},
		{Coordinate{39.465533, -87.4023442}, 0.01, Coordinate{39.47, -87.4}},
	}
	for _, tt := range tests {
		if got := Round(tt.in, tt.precision); got != tt.want {
			t.Errorf("Round(%v, %v) = %v, want %v", tt.in, tt.precision, got, tt.want)
		}
	}
	// Symmetric for negative values.
	if a, b := Round(Coordinate{1.234, 1.234}, 0.01), Round(Coordinate{-1.234, -1.234}, 0.01); a.Lat != -b.Lat || a.Lon != -b.Lon {
		t.Errorf("rounding not symmetric: %v vs %v", a, b)
	}
}

func TestCountUnique(t *testing.T) {
	coords := []Coordinate{{41.7031, -72.8312}, {41.7044, -72.8329}, {41.7031, -72.8312}, {39.4655, -87.4023}}
	if n := CountUnique(coords, 0); n != 3 {
		t.Errorf("exact: %d, want 3", n)
	}
	if n := CountUnique(coords, 0.01); n != 2 {
		t.Errorf("0.01°: %d, want 2", n)
	}
}

func TestNewRejectsBadPrecision(t *testing.T) {
	for _, p := range []float64{-0.01, 1.5} {
		if _, err := New(Config{CoordinatePrecision: p}); err == nil {
			t.Errorf("precision %v: expected error", p)
		}
	}
}
