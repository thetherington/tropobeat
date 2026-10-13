// Package lrf models LRF sites and parses them from CSV.
package lrf

// LRF is a single LRF site, as listed in the "LRF Addresses" CSV.
type LRF struct {
	DMAName   string  `json:"dma_name"`
	Code      string  `json:"code"`
	Street    string  `json:"street"`
	City      string  `json:"city"`
	State     string  `json:"state"`
	ZipCode   string  `json:"zip_code"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// GPSDMS is the degrees/minutes/seconds form of the coordinates as it
	// appears in the source file, e.g. `32° 26' 41.84" N, 99° 42' 50.95" W`.
	GPSDMS string `json:"gps_dms"`
}
