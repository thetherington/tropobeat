// Package fcc fetches TV transmitter records from the FCC TV Query service
// and models them for storage in Elasticsearch.
package fcc

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultIndex is the default Elasticsearch index for FCC transmitters.
const DefaultIndex = "fcc-transmitters"

// Transmitter is one transmitter record from the FCC TV Query listing.
// Coordinates are NAD 83, which is within a few metres of WGS84.
type Transmitter struct {
	CallSign   string
	FacilityID string
	// Service is the FCC service code, e.g. DTV (full power), DCA (Class A),
	// LPD (low power), DTS (distributed transmission site), DRT (digital
	// replacement translator).
	Service string
	// Status is the record status, e.g. LIC (licensed), CP (construction
	// permit), APP (application), AMD (amendment).
	Status string
	// RFChannel is the physical transmit channel.
	RFChannel int
	// VirtualChannel is the channel number viewers see, if assigned.
	VirtualChannel *int
	City           string
	State          string
	Country        string
	// Antenna is ND (non-directional) or DA (directional).
	Antenna    string
	FileNumber string
	Licensee   string
	// ERPkW is the effective radiated power in kilowatts.
	ERPkW *float64
	// HAATm is the antenna height above average terrain in metres.
	HAATm *float64
	// RCAMSLm is the antenna radiation centre above mean sea level in metres.
	RCAMSLm *float64
	// AntennaID is the FCC's ID for the antenna record (one per transmitter
	// site), used as a stable tie-break when numbering a station's sites.
	AntennaID string
	Latitude  float64
	Longitude float64
}

// DocID is the Elasticsearch document ID for t. It includes the location so
// that multiple sites sharing a facility ID are stored separately.
func (t Transmitter) DocID() string {
	return fmt.Sprintf("%s:%.6f,%.6f", t.FacilityID, t.Latitude, t.Longitude)
}

// Filter selects transmitters by status and service. An empty list matches
// everything. Matching is case-insensitive.
type Filter struct {
	Statuses []string
	Services []string
}

// DefaultFilter keeps licensed full-power stations: main transmitters (DTV,
// and the legacy TV and DT codes) and distributed transmission sites (DTS),
// which some full-power stations use instead of, or as well as, a single
// main transmitter.
var DefaultFilter = Filter{
	Statuses: []string{"LIC"},
	Services: []string{"DTV", "TV", "DT", "DTS"},
}

// Match reports whether t passes the filter.
func (f Filter) Match(t Transmitter) bool {
	return matchAny(f.Statuses, t.Status) && matchAny(f.Services, t.Service)
}

func matchAny(list []string, v string) bool {
	return len(list) == 0 || slices.ContainsFunc(list, func(s string) bool { return strings.EqualFold(s, v) })
}

// IndexBody is the settings and mappings used to create the transmitter
// index. Transmitter locations are stored as a geo_point in "location".
const IndexBody = `{
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "call_sign":       { "type": "keyword" },
      "facility_id":     { "type": "keyword" },
      "service":         { "type": "keyword" },
      "status":          { "type": "keyword" },
      "rf_channel":      { "type": "integer" },
      "virtual_channel": { "type": "integer" },
      "city":            { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
      "state":           { "type": "keyword" },
      "country":         { "type": "keyword" },
      "antenna":         { "type": "keyword" },
      "file_number":     { "type": "keyword" },
      "licensee":        { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
      "erp_kw":          { "type": "float" },
      "haat_m":          { "type": "float" },
      "rcamsl_m":        { "type": "float" },
      "antenna_id":      { "type": "keyword" },
      "location":        { "type": "geo_point" },
      "fetched_at":      { "type": "date" }
    }
  }
}`

// GeoPoint is an Elasticsearch geo_point in object form.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Document is the Elasticsearch representation of a Transmitter.
type Document struct {
	CallSign       string    `json:"call_sign"`
	FacilityID     string    `json:"facility_id"`
	Service        string    `json:"service"`
	Status         string    `json:"status"`
	RFChannel      int       `json:"rf_channel"`
	VirtualChannel *int      `json:"virtual_channel,omitempty"`
	City           string    `json:"city"`
	State          string    `json:"state"`
	Country        string    `json:"country"`
	Antenna        string    `json:"antenna,omitempty"`
	FileNumber     string    `json:"file_number,omitempty"`
	Licensee       string    `json:"licensee,omitempty"`
	ERPkW          *float64  `json:"erp_kw,omitempty"`
	HAATm          *float64  `json:"haat_m,omitempty"`
	RCAMSLm        *float64  `json:"rcamsl_m,omitempty"`
	AntennaID      string    `json:"antenna_id,omitempty"`
	Location       GeoPoint  `json:"location"`
	FetchedAt      time.Time `json:"fetched_at"`
}

// Document converts t to its Elasticsearch document form.
func (t Transmitter) Document(fetchedAt time.Time) Document {
	return Document{
		CallSign:       t.CallSign,
		FacilityID:     t.FacilityID,
		Service:        t.Service,
		Status:         t.Status,
		RFChannel:      t.RFChannel,
		VirtualChannel: t.VirtualChannel,
		City:           t.City,
		State:          t.State,
		Country:        t.Country,
		Antenna:        t.Antenna,
		FileNumber:     t.FileNumber,
		Licensee:       t.Licensee,
		ERPkW:          t.ERPkW,
		HAATm:          t.HAATm,
		RCAMSLm:        t.RCAMSLm,
		AntennaID:      t.AntennaID,
		Location:       GeoPoint{Lat: t.Latitude, Lon: t.Longitude},
		FetchedAt:      fetchedAt,
	}
}
