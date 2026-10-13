package cochannel

import (
	"math"
	"testing"
)

func TestMidpointKnownDistance(t *testing.T) {
	// Example from the tidwall/geodesic README: about 165,330 m.
	_, _, d, _ := Midpoint(33.4911, -112.4223, 32.1189, -113.1123)
	if math.Abs(d-165330) > 10 {
		t.Errorf("distance = %.1f m, want about 165330", d)
	}
}

func TestMidpointIsEquidistant(t *testing.T) {
	// TERH LRF to WTHI-TV, and a long north-south path.
	cases := [][4]float64{
		{39.465533, -87.4023442, 39.242417, -87.384917},
		{41.800928, -71.343793, 44.0, -73.0},
		{18.464969, -66.097109, 18.2, -67.1},
	}
	for _, c := range cases {
		midLat, midLon, d, _ := Midpoint(c[0], c[1], c[2], c[3])
		d1 := Distance(c[0], c[1], midLat, midLon)
		d2 := Distance(midLat, midLon, c[2], c[3])
		if math.Abs(d1-d2) > 1 || math.Abs(d1+d2-d) > 1 {
			t.Errorf("%v: halves %.2f m and %.2f m, total %.2f m", c, d1, d2, d)
		}
	}
}

func TestMidpointBearing(t *testing.T) {
	// Due north and due east from the equator.
	if _, _, _, b := Midpoint(0, 0, 1, 0); math.Abs(b) > 1e-6 {
		t.Errorf("north bearing = %v, want 0", b)
	}
	if _, _, _, b := Midpoint(0, 0, 0, 1); math.Abs(b-90) > 1e-6 {
		t.Errorf("east bearing = %v, want 90", b)
	}
	// West gives 270, not -90.
	if _, _, _, b := Midpoint(0, 0, 0, -1); math.Abs(b-270) > 1e-6 {
		t.Errorf("west bearing = %v, want 270", b)
	}
}
