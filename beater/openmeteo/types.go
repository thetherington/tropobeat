package openmeteo

import (
	"fmt"
	"strconv"
	"time"
)

// Coordinate is a WGS84 latitude/longitude pair in decimal degrees.
type Coordinate struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func (c Coordinate) String() string {
	return strconv.FormatFloat(c.Lat, 'f', -1, 64) + "," + strconv.FormatFloat(c.Lon, 'f', -1, 64)
}

// HourlySeries holds the raw hourly variables returned for one coordinate.
type HourlySeries struct {
	// Requested is the coordinate that was asked for.
	Requested Coordinate
	// Grid is the model grid point Open-Meteo actually used, which is
	// usually slightly different from Requested.
	Grid Coordinate
	// Elevation is the terrain elevation in metres used by Open-Meteo.
	Elevation float64
	// Times are the UTC timestamps of each hourly value.
	Times []time.Time
	// Values maps each requested variable name to its hourly values, aligned
	// with Times. A nil entry means the API returned no value for that hour.
	Values map[string][]*float64
}

// Profile is a vertical snapshot of the atmosphere at one coordinate and
// hour: the surface plus each requested pressure level.
//
// Levels are passed through exactly as Open-Meteo returns them. At
// high-elevation sites, some pressure levels are extrapolated below the
// terrain; compare Level.HeightM with Elevation to filter them.
type Profile struct {
	Requested Coordinate
	Grid      Coordinate
	// Elevation is the terrain elevation in metres above sea level.
	Elevation float64
	// Time is the UTC hour this profile describes.
	Time time.Time
	// Surface holds the 2 m temperature and humidity and the surface
	// pressure, at a height of Elevation + 2 m.
	Surface Level
	// Levels are the pressure levels in descending pressure order, i.e.
	// increasing height.
	Levels []Level
}

// Level is the state of the atmosphere at one height.
type Level struct {
	// PressureHPa is the atmospheric pressure in hectopascals. For the
	// surface level it is nil if the API returned no surface pressure.
	PressureHPa *float64
	// HeightM is the height in metres above sea level (geopotential height
	// for pressure levels).
	HeightM *float64
	// TemperatureK is the air temperature in Kelvin.
	TemperatureK *float64
	// RelativeHumidityPct is the relative humidity in percent.
	RelativeHumidityPct *float64
}

// APIError is an error response from the Open-Meteo API.
type APIError struct {
	StatusCode int
	Reason     string
}

func (e *APIError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("open-meteo: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("open-meteo: HTTP %d: %s", e.StatusCode, e.Reason)
}

// BatchError reports that a batch request failed for a set of coordinates.
type BatchError struct {
	Coordinates []Coordinate
	Err         error
}

func (e *BatchError) Error() string {
	return fmt.Sprintf("open-meteo: batch of %d coordinates failed: %v", len(e.Coordinates), e.Err)
}

func (e *BatchError) Unwrap() error { return e.Err }

// CoordinateError reports a problem with the data for a single coordinate.
type CoordinateError struct {
	Coordinate Coordinate
	Err        error
}

func (e *CoordinateError) Error() string {
	return fmt.Sprintf("open-meteo: %s: %v", e.Coordinate, e.Err)
}

func (e *CoordinateError) Unwrap() error { return e.Err }
