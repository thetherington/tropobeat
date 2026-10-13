package fcc

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Column positions in the TV Query pipe-delimited output (list=4). Verified
// against the FCC's labelled station page; see testdata/tvq_wjar_list0.html.
const (
	colCallSign       = 0
	colService        = 2
	colRFChannel      = 3
	colAntenna        = 4
	colStatus         = 8
	colCity           = 9
	colState          = 10
	colCountry        = 11
	colFileNumber     = 12
	colERP            = 13
	colHAAT           = 15
	colFacilityID     = 17
	colLatDir         = 18 // followed by degrees, minutes, seconds
	colLonDir         = 22 // followed by degrees, minutes, seconds
	colLicensee       = 26
	colRCAMSL         = 30
	colVirtualChannel = 37

	minColumns = colVirtualChannel + 1
)

// ParseTVQ parses TV Query list=4 output. Records that can't be used, such
// as those with missing coordinates or channel, are skipped and counted. A
// line with too few columns means the format has changed (or the response
// isn't a listing at all), and is reported as an error.
func ParseTVQ(r io.Reader) (transmitters []Transmitter, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		text = strings.TrimPrefix(text, "|")

		cols := strings.Split(text, "|")
		if len(cols) < minColumns {
			return nil, 0, fmt.Errorf("line %d: got %d columns, want at least %d: unexpected TV Query format", line, len(cols), minColumns)
		}
		for i := range cols {
			cols[i] = strings.TrimSpace(cols[i])
		}

		t, ok := parseRecord(cols)
		if !ok {
			skipped++
			continue
		}
		transmitters = append(transmitters, t)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	return transmitters, skipped, nil
}

func parseRecord(cols []string) (Transmitter, bool) {
	channel, err := strconv.Atoi(cols[colRFChannel])
	if err != nil || cols[colCallSign] == "" {
		return Transmitter{}, false
	}
	lat, err := parseDMS(cols[colLatDir : colLatDir+4])
	if err != nil {
		return Transmitter{}, false
	}
	lon, err := parseDMS(cols[colLonDir : colLonDir+4])
	if err != nil {
		return Transmitter{}, false
	}

	t := Transmitter{
		CallSign:   cols[colCallSign],
		FacilityID: cols[colFacilityID],
		Service:    cols[colService],
		Status:     cols[colStatus],
		RFChannel:  channel,
		City:       cols[colCity],
		State:      cols[colState],
		Country:    cols[colCountry],
		Antenna:    dash(cols[colAntenna]),
		FileNumber: dash(cols[colFileNumber]),
		Licensee:   dash(cols[colLicensee]),
		ERPkW:      parseQuantity(cols[colERP], "kW"),
		HAATm:      parseQuantity(cols[colHAAT], ""),
		RCAMSLm:    parseQuantity(cols[colRCAMSL], "m"),
		Latitude:   lat,
		Longitude:  lon,
	}
	if v, err := strconv.Atoi(cols[colVirtualChannel]); err == nil {
		t.VirtualChannel = &v
	}
	return t, true
}

// parseDMS converts [direction, degrees, minutes, seconds] to signed decimal
// degrees. South and West are negative.
func parseDMS(f []string) (float64, error) {
	dir := strings.ToUpper(f[0])
	deg, err1 := strconv.ParseFloat(f[1], 64)
	min, err2 := strconv.ParseFloat(f[2], 64)
	sec, err3 := strconv.ParseFloat(f[3], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, fmt.Errorf("invalid DMS %q", f)
	}
	if min < 0 || min >= 60 || sec < 0 || sec >= 60 || deg < 0 {
		return 0, fmt.Errorf("DMS out of range %q", f)
	}

	v := deg + min/60 + sec/3600
	switch dir {
	case "N", "E":
	case "S", "W":
		v = -v
	default:
		return 0, fmt.Errorf("invalid direction %q", dir)
	}

	limit := 90.0
	if dir == "E" || dir == "W" {
		limit = 180
	}
	if v < -limit || v > limit {
		return 0, fmt.Errorf("coordinate out of range %q", f)
	}
	return v, nil
}

// parseQuantity parses values like "1000.  kW", "331.   m" or "302.0". If
// unit is set, a value with a different unit is rejected. "-" and empty
// values return nil.
func parseQuantity(s, unit string) *float64 {
	fields := strings.Fields(s)
	if len(fields) == 0 || fields[0] == "-" {
		return nil
	}
	if len(fields) > 1 && unit != "" && fields[1] != unit {
		return nil
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	return &v
}

func dash(s string) string {
	if s == "-" {
		return ""
	}
	return s
}
