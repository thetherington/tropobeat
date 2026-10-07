package openmeteo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// timeLayout is the ISO 8601 format Open-Meteo uses for hourly times.
const timeLayout = "2006-01-02T15:04"

// location is one entry of an Open-Meteo forecast response.
type location struct {
	Latitude  float64                    `json:"latitude"`
	Longitude float64                    `json:"longitude"`
	Elevation float64                    `json:"elevation"`
	Hourly    map[string]json.RawMessage `json:"hourly"`
}

// errorResponse is the body Open-Meteo returns for a rejected request.
type errorResponse struct {
	Error  bool   `json:"error"`
	Reason string `json:"reason"`
}

// decodeLocations decodes a forecast response body. Open-Meteo returns a
// single object for one coordinate and an array for several.
func decodeLocations(body []byte) ([]location, error) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var locs []location
		if err := json.Unmarshal(body, &locs); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}
		return locs, nil
	}

	var loc location
	if err := json.Unmarshal(body, &loc); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return []location{loc}, nil
}

// series converts a decoded location into an HourlySeries for vars.
func (l location) series(requested Coordinate, vars []string) (HourlySeries, error) {
	s := HourlySeries{
		Requested: requested,
		Grid:      Coordinate{Lat: l.Latitude, Lon: l.Longitude},
		Elevation: l.Elevation,
		Values:    make(map[string][]*float64, len(vars)),
	}

	var times []string
	if raw, ok := l.Hourly["time"]; ok {
		if err := json.Unmarshal(raw, &times); err != nil {
			return s, fmt.Errorf("decoding hourly time: %w", err)
		}
	}
	s.Times = make([]time.Time, len(times))
	for i, t := range times {
		parsed, err := time.ParseInLocation(timeLayout, t, time.UTC)
		if err != nil {
			return s, fmt.Errorf("parsing hourly time %q: %w", t, err)
		}
		s.Times[i] = parsed
	}

	for _, v := range vars {
		raw, ok := l.Hourly[v]
		if !ok {
			return s, fmt.Errorf("variable %q missing from response", v)
		}
		var vals []*float64
		if err := json.Unmarshal(raw, &vals); err != nil {
			return s, fmt.Errorf("decoding variable %q: %w", v, err)
		}
		if len(vals) != len(s.Times) {
			return s, fmt.Errorf("variable %q has %d values for %d times", v, len(vals), len(s.Times))
		}
		s.Values[v] = vals
	}

	return s, nil
}
