package cochannel

import (
	"fmt"
	"math"
	"strconv"

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
	// AntennaID is the FCC antenna record ID of this site, when known.
	AntennaID string
	// Site and Sites number a station's transmitter sites on this channel
	// (distributed transmission): site Site of Sites, strongest first. Both
	// are 1 for a station with a single site.
	Site, Sites int
}

// MultiSite reports whether the station uses more than one transmitter site
// on this channel.
func (t Transmitter) MultiSite() bool { return t.Sites > 1 }

// Label names the transmitter for people: the call sign, or for a station
// with several sites, which site it is and its power, e.g.
// "WVER site 2 of 4 (5 kW)".
func (t Transmitter) Label() string {
	if !t.MultiSite() {
		return t.CallSign
	}
	l := fmt.Sprintf("%s site %d of %d", t.CallSign, t.Site, t.Sites)
	if t.ERPkW != nil {
		l += " (" + strconv.FormatFloat(math.Round(*t.ERPkW*100)/100, 'f', -1, 64) + " kW)"
	}
	return l
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

// ID identifies the path: "<lrf code>:<facility id>:<rf channel>", with
// ":s<site>" appended when the station has several transmitter sites on the
// channel (distributed transmission), e.g. "HART:69946:10:s2". The sites
// share a facility ID and channel, so without the suffix their paths would
// collide; single-site IDs keep the plain form.
func (p Path) ID() string {
	id := fmt.Sprintf("%s:%s:%d", p.LRFCode, p.Transmitter.FacilityID, p.RFChannel)
	if p.Transmitter.MultiSite() {
		id += fmt.Sprintf(":s%d", p.Transmitter.Site)
	}
	return id
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
