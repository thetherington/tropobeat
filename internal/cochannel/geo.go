// Package cochannel discovers co-channel paths: for each LRF and RF channel
// it receives, the same-channel FCC transmitters within a search radius,
// excluding the LRF's own station, with the geodesic midpoint of each path.
package cochannel

import (
	"math"

	"github.com/tidwall/geodesic"
)

// metresPerMile converts statute miles to metres.
const metresPerMile = 1609.344

// Midpoint returns the point halfway along the WGS84 geodesic from
// (lat1, lon1) to (lat2, lon2), the geodesic distance in metres, and the
// initial bearing from the first point in degrees clockwise from north
// (0–360).
func Midpoint(lat1, lon1, lat2, lon2 float64) (midLat, midLon, distM, bearingDeg float64) {
	var azi1 float64
	geodesic.WGS84.Inverse(lat1, lon1, lat2, lon2, &distM, &azi1, nil)
	geodesic.WGS84.Direct(lat1, lon1, azi1, distM/2, &midLat, &midLon, nil)
	return midLat, midLon, distM, math.Mod(azi1+360, 360)
}

// Distance returns the WGS84 geodesic distance in metres between two points.
func Distance(lat1, lon1, lat2, lon2 float64) float64 {
	var d float64
	geodesic.WGS84.Inverse(lat1, lon1, lat2, lon2, &d, nil, nil)
	return d
}
