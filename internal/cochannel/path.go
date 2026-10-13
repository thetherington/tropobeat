package cochannel

import (
	"fmt"

	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// Point labels used in weather groups built from paths.
const (
	LabelLRF         = "lrf"
	LabelMidpoint    = "midpoint"
	LabelTransmitter = "transmitter"
)

// Point is a WGS84 location in decimal degrees.
type Point struct {
	Lat float64
	Lon float64
}

// Transmitter is the co-channel transmitter at the far end of a path.
type Transmitter struct {
	CallSign   string
	FacilityID string
	Service    string
	ERPkW      *float64
	Location   Point
}

// Path is a co-channel path: an LRF, a same-channel transmitter within the
// search radius that is not the LRF's own station, and the geodesic midpoint
// between them.
type Path struct {
	LRFCode string
	LRF     Point
	// LRFCallSigns are the call signs the LRF receives on RFChannel, taken
	// from the demod input tags.
	LRFCallSigns []string
	RFChannel    int
	Transmitter  Transmitter
	Midpoint     Point
	// DistanceMi is the geodesic length of the path in statute miles.
	DistanceMi float64
	// BearingDeg is the initial bearing from the LRF towards the
	// transmitter, in degrees clockwise from north.
	BearingDeg float64
}

// ID identifies the path: "<lrf code>:<facility id>:<rf channel>".
func (p Path) ID() string {
	return fmt.Sprintf("%s:%s:%d", p.LRFCode, p.Transmitter.FacilityID, p.RFChannel)
}

// Group returns the path as a weather group with its LRF, midpoint and
// transmitter points, in that order.
func (p Path) Group() openmeteo.Group {
	return openmeteo.Group{
		ID: p.ID(),
		Points: []openmeteo.Point{
			{Label: LabelLRF, Coordinate: openmeteo.Coordinate{Lat: p.LRF.Lat, Lon: p.LRF.Lon}},
			{Label: LabelMidpoint, Coordinate: openmeteo.Coordinate{Lat: p.Midpoint.Lat, Lon: p.Midpoint.Lon}},
			{Label: LabelTransmitter, Coordinate: openmeteo.Coordinate{Lat: p.Transmitter.Location.Lat, Lon: p.Transmitter.Location.Lon}},
		},
	}
}
