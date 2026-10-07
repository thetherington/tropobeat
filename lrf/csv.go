package lrf

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CSV column headers, normalized with normalizeHeader.
const (
	colDMAName = "dma name"
	colCode    = "code"
	colStreet  = "physical street address"
	colCity    = "physical city"
	colState   = "state"
	colZip     = "zip code"
	colLatLon  = "physical city lattitude and longitude"
	colGPSDMS  = "physical city gps coordinates"
)

var requiredColumns = []string{colDMAName, colCode, colStreet, colCity, colState, colZip, colLatLon}

// ReadFile parses the LRF CSV file at path.
func ReadFile(path string) ([]LRF, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return Parse(f)
}

// Parse reads LRF records from CSV. Columns are matched by header name, so
// their order does not matter. Input may be UTF-8 or Latin-1 (as exported
// by Excel).
func Parse(r io.Reader) ([]LRF, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	cr := csv.NewReader(bytes.NewReader(toUTF8(data)))
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}

	cols := make(map[string]int, len(header))
	for i, h := range header {
		cols[normalizeHeader(h)] = i
	}
	for _, c := range requiredColumns {
		if _, ok := cols[c]; !ok {
			return nil, fmt.Errorf("missing required column %q", c)
		}
	}

	var lrfs []LRF
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := cr.FieldPos(0)

		get := func(col string) string {
			i, ok := cols[col]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}

		lat, lon, err := parseLatLon(get(colLatLon))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		lrfs = append(lrfs, LRF{
			DMAName:   get(colDMAName),
			Code:      get(colCode),
			Street:    get(colStreet),
			City:      get(colCity),
			State:     get(colState),
			ZipCode:   normalizeZip(get(colZip)),
			Latitude:  lat,
			Longitude: lon,
			GPSDMS:    get(colGPSDMS),
		})
	}

	return lrfs, nil
}

// parseLatLon parses a "lat, lon" pair such as "32.4449556, -99.7141538".
func parseLatLon(s string) (lat, lon float64, err error) {
	latStr, lonStr, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, fmt.Errorf("invalid coordinates %q: expected \"lat, lon\"", s)
	}
	if lat, err = strconv.ParseFloat(strings.TrimSpace(latStr), 64); err != nil {
		return 0, 0, fmt.Errorf("invalid latitude in %q: %w", s, err)
	}
	if lon, err = strconv.ParseFloat(strings.TrimSpace(lonStr), 64); err != nil {
		return 0, 0, fmt.Errorf("invalid longitude in %q: %w", s, err)
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, fmt.Errorf("coordinates out of range: %q", s)
	}
	return lat, lon, nil
}

// normalizeZip restores leading zeros that spreadsheets strip from numeric
// ZIP codes (e.g. "4401" -> "04401").
func normalizeZip(zip string) string {
	if zip != "" && len(zip) < 5 {
		if _, err := strconv.Atoi(zip); err == nil {
			return strings.Repeat("0", 5-len(zip)) + zip
		}
	}
	return zip
}

// normalizeHeader lowercases h and collapses runs of whitespace.
func normalizeHeader(h string) string {
	h = strings.TrimPrefix(h, "\ufeff")
	return strings.ToLower(strings.Join(strings.Fields(h), " "))
}

// toUTF8 returns data unchanged if it is valid UTF-8, otherwise decodes it
// as Latin-1.
func toUTF8(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	buf := make([]rune, len(data))
	for i, b := range data {
		buf[i] = rune(b)
	}
	return []byte(string(buf))
}
