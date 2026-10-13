package fcc

import (
	"html"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

func loadRI(t *testing.T) []Transmitter {
	t.Helper()
	f, err := os.Open("testdata/tvq_ri.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ts, skipped, err := ParseTVQ(f)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("skipped %d records, want 0", skipped)
	}
	return ts
}

func find(ts []Transmitter, call, status string) *Transmitter {
	for i := range ts {
		if ts[i].CallSign == call && ts[i].Status == status {
			return &ts[i]
		}
	}
	return nil
}

func TestParseTVQRhodeIsland(t *testing.T) {
	ts := loadRI(t)
	if len(ts) != 19 {
		t.Fatalf("parsed %d records, want 19", len(ts))
	}

	w := find(ts, "WJAR", "LIC")
	if w == nil {
		t.Fatal("WJAR not found")
	}
	if w.Service != "DTV" || w.RFChannel != 25 || w.VirtualChannel == nil || *w.VirtualChannel != 10 {
		t.Errorf("WJAR service/channels = %s RF %d virtual %v", w.Service, w.RFChannel, w.VirtualChannel)
	}
	if w.FacilityID != "50780" || w.FileNumber != "0000087546" || w.Licensee != "WJAR LICENSEE, LLC" {
		t.Errorf("WJAR identifiers = %q %q %q", w.FacilityID, w.FileNumber, w.Licensee)
	}
	if w.City != "PROVIDENCE" || w.State != "RI" || w.Country != "US" || w.Antenna != "ND" {
		t.Errorf("WJAR location fields = %q %q %q %q", w.City, w.State, w.Country, w.Antenna)
	}
	if *w.ERPkW != 1000 || *w.HAATm != 302 || *w.RCAMSLm != 331 {
		t.Errorf("WJAR ERP/HAAT/RCAMSL = %v %v %v", *w.ERPkW, *w.HAATm, *w.RCAMSLm)
	}
	if math.Abs(w.Latitude-41.865389) > 1e-6 || math.Abs(w.Longitude-(-71.286861)) > 1e-6 {
		t.Errorf("WJAR coordinates = %v, %v", w.Latitude, w.Longitude)
	}
	if got := w.DocID(); got != "50780:41.865389,-71.286861" {
		t.Errorf("DocID = %q", got)
	}

	// Records without a virtual channel or with "-" placeholders.
	n := find(ts, "NEW", "CP")
	if n == nil || n.VirtualChannel != nil || n.Licensee == "-" {
		t.Errorf("NEW CP record = %+v", n)
	}
}

func TestDefaultFilterRhodeIsland(t *testing.T) {
	var kept []string
	for _, tr := range loadRI(t) {
		if DefaultFilter.Match(tr) {
			kept = append(kept, tr.CallSign)
		}
	}
	want := "WSBE-TV WPRI-TV WNAC-TV WPXQ-TV WJAR"
	if got := strings.Join(kept, " "); got != want {
		t.Errorf("kept %q, want %q", got, want)
	}
}

// TestColumnMappingMatchesFCCLabels checks the list=4 column mapping
// against the FCC's labelled station page for WJAR (list=0).
func TestColumnMappingMatchesFCCLabels(t *testing.T) {
	raw, err := os.ReadFile("testdata/tvq_wjar_list0.html")
	if err != nil {
		t.Fatal(err)
	}
	page := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(string(raw), " ")
	page = strings.Join(strings.Fields(html.UnescapeString(page)), " ")

	w := find(loadRI(t), "WJAR", "LIC")
	labels := map[string]string{
		`Transmit Channel: 25 `:         "RF channel (col 3)",
		`Licensed Virtual Channel: 10 `: "virtual channel (col 37)",
		`Facility ID No: 50780`:         "facility ID (col 17)",
		`LMS File Number: 0000087546`:   "file number (col 12)",
		`Licensee: WJAR LICENSEE, LLC`:  "licensee (col 26)",
		`(ERP): 1000. kW ERP`:           "ERP (col 13)",
		`302. meters HAAT`:              "HAAT (col 15)",
		`p_rcamsl_max = '331.'`:         "RCAMSL (col 30)",
		`41.865389 -71.286861`:          "coordinates (cols 18-25)",
		`Service Designation: DTV`:      "service (col 2)",
		`c_dom_status = 'LIC'`:          "status (col 8)",
	}
	for label, field := range labels {
		if !strings.Contains(page, label) {
			t.Errorf("FCC page does not contain %q (%s)", label, field)
		}
	}

	// The parsed values must agree with those labels.
	if w.RFChannel != 25 || *w.VirtualChannel != 10 || w.FacilityID != "50780" || w.FileNumber != "0000087546" ||
		w.Licensee != "WJAR LICENSEE, LLC" || *w.ERPkW != 1000 || *w.HAATm != 302 || *w.RCAMSLm != 331 ||
		w.Service != "DTV" || w.Status != "LIC" {
		t.Errorf("parsed WJAR does not match labelled values: %+v", w)
	}
}

func TestParseTVQRejectsUnexpectedFormat(t *testing.T) {
	_, _, err := ParseTVQ(strings.NewReader("<html><body>Service unavailable</body></html>\n"))
	if err == nil || !strings.Contains(err.Error(), "unexpected TV Query format") {
		t.Fatalf("err = %v, want format error", err)
	}
}

func TestParseTVQEmpty(t *testing.T) {
	ts, skipped, err := ParseTVQ(strings.NewReader("\n\n"))
	if err != nil || len(ts) != 0 || skipped != 0 {
		t.Fatalf("got %d, %d, %v; want empty result", len(ts), skipped, err)
	}
}

func TestParseTVQSkipsBadCoordinates(t *testing.T) {
	good := "|WJAR |- |DTV|25 |ND |- |1 |- |LIC |PROVIDENCE |RI |US |0000087546|1000. kW |- |302.0 |- |50780 |N |41 |51 |55.4 |W |71 |17 |12.7 |WJAR LICENSEE, LLC |0 km |0 mi |0 deg |331. m| |- |- |1005123 |270.7 |87546 |10 |a |b |"
	bad := strings.Replace(good, "|N |41 |51 |55.4 |", "|- |- |- |- |", 1)
	ts, skipped, err := ParseTVQ(strings.NewReader(good + "\n" + bad + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || skipped != 1 {
		t.Errorf("got %d records, %d skipped; want 1, 1", len(ts), skipped)
	}
}

func TestParseDMS(t *testing.T) {
	tests := []struct {
		in      []string
		want    float64
		wantErr bool
	}{
		{[]string{"N", "41", "51", "55.4"}, 41.865389, false},
		{[]string{"W", "71", "17", "12.7"}, -71.286861, false},
		{[]string{"S", "33", "52", "0"}, -33.866667, false},
		{[]string{"E", "151", "12", "36"}, 151.21, false},
		{[]string{"X", "1", "2", "3"}, 0, true},
		{[]string{"N", "91", "0", "0"}, 0, true},
		{[]string{"N", "41", "60", "0"}, 0, true},
		{[]string{"N", "-", "-", "-"}, 0, true},
	}
	for _, tt := range tests {
		got, err := parseDMS(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseDMS(%v) err = %v", tt.in, err)
			continue
		}
		if !tt.wantErr && math.Abs(got-tt.want) > 1e-6 {
			t.Errorf("parseDMS(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseQuantity(t *testing.T) {
	tests := []struct {
		in, unit string
		want     *float64
	}{
		{"1000.  kW", "kW", ptr(1000)},
		{".25    kW", "kW", ptr(0.25)},
		{"331.   m", "m", ptr(331)},
		{"302.0", "", ptr(302)},
		{"-", "", nil},
		{"", "kW", nil},
		{"5 W", "kW", nil},
	}
	for _, tt := range tests {
		got := parseQuantity(tt.in, tt.unit)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseQuantity(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func ptr(v float64) *float64 { return &v }
