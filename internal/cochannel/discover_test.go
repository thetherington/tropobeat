package cochannel

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v8"

	"github.com/thetherington/tropobeat/internal/elastic"
	"github.com/thetherington/tropobeat/internal/fcc"
	"github.com/thetherington/tropobeat/internal/lrf"
)

// Test LRFs (coordinates from the LRF list).
var (
	terh = lrf.LRF{Code: "TERH", Latitude: 39.465533, Longitude: -87.4023442}
	prov = lrf.LRF{Code: "PROV", Latitude: 41.800928, Longitude: -71.343793}
)

// Test transmitters.
var transmitters = []fcc.Document{
	// WMULTI: one station (facility 3001) with three DTS sites on RF 10
	// within 200 mi of TERH, listed out of ERP order on purpose.
	{CallSign: "WMULTI", FacilityID: "3001", Service: "DTS", RFChannel: 10, ERPkW: ptrF(5), AntennaID: "a2", Location: fcc.GeoPoint{Lat: 40.8, Lon: -86.5}},
	{CallSign: "WMULTI", FacilityID: "3001", Service: "DTS", RFChannel: 10, ERPkW: ptrF(15), AntennaID: "a1", Location: fcc.GeoPoint{Lat: 40.9, Lon: -86.4}},
	{CallSign: "WMULTI", FacilityID: "3001", Service: "DTS", RFChannel: 10, ERPkW: ptrF(0.32), AntennaID: "a3", Location: fcc.GeoPoint{Lat: 40.7, Lon: -86.6}},
	// RF 2 at Providence: WSBE-TV and WRIW-CD share one transmitter
	// (channel sharing); WNRB is a separate site 1.5 km away; WFAR2 is a
	// genuine co-channel station about 60 mi away.
	{CallSign: "WSBE-TV", FacilityID: "56092", Service: "DTV", RFChannel: 2, Location: fcc.GeoPoint{Lat: 41.865389, Lon: -71.286861}},
	{CallSign: "WRIW-CD", FacilityID: "70184", Service: "DCA", RFChannel: 2, Location: fcc.GeoPoint{Lat: 41.865389, Lon: -71.286861}},
	{CallSign: "WNRB", FacilityID: "2001", Service: "DTV", RFChannel: 2, Location: fcc.GeoPoint{Lat: 41.865389, Lon: -71.268800}},
	{CallSign: "WFAR2", FacilityID: "2002", Service: "DTV", RFChannel: 2, Location: fcc.GeoPoint{Lat: 42.4, Lon: -72.3}},
	{CallSign: "WTHI-TV", FacilityID: "70655", Service: "DTV", RFChannel: 10, Location: fcc.GeoPoint{Lat: 39.242417, Lon: -87.384917}}, // TERH's own, 15 mi
	{CallSign: "WFAR-TV", FacilityID: "1001", Service: "DTV", RFChannel: 10, Location: fcc.GeoPoint{Lat: 40.4, Lon: -86.0}},            // ~100 mi from TERH
	{CallSign: "WOUT", FacilityID: "1002", Service: "DTV", RFChannel: 10, Location: fcc.GeoPoint{Lat: 44.5, Lon: -87.4}},               // ~350 mi, outside radius
	{CallSign: "WJAR", FacilityID: "50780", Service: "DTV", RFChannel: 25, Location: fcc.GeoPoint{Lat: 41.865389, Lon: -71.286861}},    // PROV's own
	{CallSign: "WABC-TV", FacilityID: "1328", Service: "DTV", RFChannel: 25, Location: fcc.GeoPoint{Lat: 40.7484, Lon: -73.9857}},      // ~150 mi from PROV
	{CallSign: "WSEE-TV", FacilityID: "1003", Service: "DTV", RFChannel: 7, Location: fcc.GeoPoint{Lat: 41.9, Lon: -72.0}},             // PROV RF 7, tag names an LPTV
	{CallSign: "KFAR", FacilityID: "1004", Service: "DTV", RFChannel: 33, Location: fcc.GeoPoint{Lat: 41.9, Lon: -71.5}},               // tagged on RF 33 at a far LRF
}

// demodBucket is one composite-aggregation bucket.
type demodBucket struct {
	LRF string  `json:"lrf"`
	RF  int     `json:"rf"`
	Tag *string `json:"tag"`
}

func strp(s string) *string { return &s }

// fakeES serves cluster info, a paged composite aggregation over buckets,
// and _msearch over transmitters (filtered by rf_channel and distance).
type fakeES struct {
	t        *testing.T
	buckets  []demodBucket
	pageSize int

	mu       sync.Mutex
	searches []map[string]any
}

func (f *fakeES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/":
		_, _ = w.Write([]byte(`{"version":{"number":"8.17.0"}}`))

	case strings.HasSuffix(r.URL.Path, "/_search"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["aggs"]; !ok {
			// SearchAll of the FCC index.
			hits := make([]any, len(transmitters))
			for i, t := range transmitters {
				hits[i] = map[string]any{"_source": t}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{
				"total": map[string]any{"value": len(hits)}, "hits": hits}})
			return
		}
		f.mu.Lock()
		f.searches = append(f.searches, body)
		f.mu.Unlock()

		start := 0
		comp := body["aggs"].(map[string]any)["pairs"].(map[string]any)["composite"].(map[string]any)
		if after, ok := comp["after"].(map[string]any); ok {
			start = int(after["i"].(float64))
		}
		end := min(start+f.pageSize, len(f.buckets))
		var out []map[string]any
		for _, b := range f.buckets[start:end] {
			out = append(out, map[string]any{"key": b, "doc_count": 1})
		}
		pairs := map[string]any{"buckets": out}
		if end < len(f.buckets) {
			pairs["after_key"] = map[string]any{"i": end}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"aggregations": map[string]any{"pairs": pairs}})

	case r.URL.Path == "/_msearch":
		var responses []any
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() { // header line
			if !sc.Scan() {
				break
			}
			var q map[string]any
			_ = json.Unmarshal(sc.Bytes(), &q)
			responses = append(responses, map[string]any{"status": 200, "hits": map[string]any{"hits": f.match(q)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeES) match(q map[string]any) []any {
	filters := q["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
	rf := int(filters[0].(map[string]any)["term"].(map[string]any)["rf_channel"].(float64))
	gd := filters[1].(map[string]any)["geo_distance"].(map[string]any)
	center := gd["location"].(map[string]any)
	radiusMi, _ := strconv.ParseFloat(strings.TrimSuffix(gd["distance"].(string), "mi"), 64)

	var hits []any
	for _, t := range transmitters {
		d := Distance(center["lat"].(float64), center["lon"].(float64), t.Location.Lat, t.Location.Lon) / metresPerMile
		if t.RFChannel == rf && d <= radiusMi {
			hits = append(hits, map[string]any{"_source": t})
		}
	}
	return hits
}

func esClient(t *testing.T, f *fakeES) *elasticsearch.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := elastic.NewClient(elastic.Config{Hosts: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadReceivedPagesAndMerges(t *testing.T) {
	f := &fakeES{t: t, pageSize: 2, buckets: []demodBucket{
		{LRF: "TERH", RF: 10, Tag: strp("WTHI / WTHI2 (OTA)")},
		{LRF: "TERH", RF: 10, Tag: strp("WTHI / WTHI2  ( OTA )")}, // spacing variant, same channel
		{LRF: "prov", RF: 25, Tag: strp("WJAR(OTA)")},
		{LRF: "PROV", RF: 7, Tag: strp("K22JA / KDF ( OTA )")},
		{LRF: "PROV", RF: 9, Tag: strp("Spare")},
		{LRF: "PROV", RF: 11, Tag: nil}, // missing tag
	}}
	es := esClient(t, f)

	got, err := LoadReceived(context.Background(), es, "log-metric-p-rfdemod-*", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.searches) != 3 {
		t.Errorf("made %d searches, want 3 pages", len(f.searches))
	}

	// The query filters on locked inputs within the window.
	q, _ := json.Marshal(f.searches[0]["query"])
	for _, want := range []string{`"rfdemod.rf_demod.s_input_status":"Locked"`, `"gte":"now-86400s"`} {
		if !strings.Contains(string(q), want) {
			t.Errorf("query %s missing %s", q, want)
		}
	}

	want := map[string]Received{
		"PROV:7":  {LRFCode: "PROV", RFChannel: 7, Tags: []string{"K22JA / KDF ( OTA )"}, CallSigns: []string{"K22JA", "KDF"}},
		"PROV:9":  {LRFCode: "PROV", RFChannel: 9, Tags: []string{"Spare"}},
		"PROV:11": {LRFCode: "PROV", RFChannel: 11},
		"PROV:25": {LRFCode: "PROV", RFChannel: 25, Tags: []string{"WJAR(OTA)"}, CallSigns: []string{"WJAR"}},
		"TERH:10": {LRFCode: "TERH", RFChannel: 10, Tags: []string{"WTHI / WTHI2 ( OTA )", "WTHI / WTHI2 (OTA)"}, CallSigns: []string{"WTHI", "WTHI2"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d channels, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		w, ok := want[r.LRFCode+":"+strconv.Itoa(r.RFChannel)]
		if !ok || !slices.Equal(r.Tags, w.Tags) || !slices.Equal(r.CallSigns, w.CallSigns) {
			t.Errorf("got %+v, want %+v", r, w)
		}
	}
}

func TestParseCallSigns(t *testing.T) {
	tests := map[string][]string{
		"WTHI / WTHI2 (OTA)":     {"WTHI", "WTHI2"},
		"WMTW(OTA)":              {"WMTW"},
		"KRBK / KOZL (OTA)":      {"KRBK", "KOZL"},
		"KOLR ( OTA )":           {"KOLR"},
		"KAKM / 2 / 3 / 4 (OTA)": {"KAKM"},
		"Spare":                  nil,
		"NA":                     nil,
		"QUINTEKA":               nil,
		"":                       nil,
		"(OTA)":                  nil,
		"CBS 10":                 nil,
		"XHAS (OTA)":             {"XHAS"},
	}
	for tag, want := range tests {
		if got := ParseCallSigns(tag); !slices.Equal(got, want) {
			t.Errorf("ParseCallSigns(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestDiscover(t *testing.T) {
	f := &fakeES{t: t}
	es := esClient(t, f)
	received := []Received{
		{LRFCode: "TERH", RFChannel: 10, Tags: []string{"WTHI / WTHI2 (OTA)"}, CallSigns: []string{"WTHI", "WTHI2"}},
		{LRFCode: "PROV", RFChannel: 25, Tags: []string{"WJAR (OTA)"}, CallSigns: []string{"WJAR"}},
		{LRFCode: "PROV", RFChannel: 7, Tags: []string{"K22JA (OTA)"}, CallSigns: []string{"K22JA"}}, // not in FCC set
		{LRFCode: "PROV", RFChannel: 9, Tags: []string{"Spare"}},                                     // no call sign
		{LRFCode: "BIRM", RFChannel: 13, Tags: []string{"WVTM (OTA)"}, CallSigns: []string{"WVTM"}},  // unknown LRF
	}

	res, err := Discover(context.Background(), es, Options{FCCIndex: "fcc-transmitters", Radius: "200mi"}, []lrf.LRF{terh, prov}, received)
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, p := range res.Paths {
		ids = append(ids, p.ID()+"/"+p.Transmitter.CallSign)
	}
	want := []string{"PROV:1328:25/WABC-TV", "PROV:1003:7/WSEE-TV", "TERH:1001:10/WFAR-TV",
		"TERH:3001:10:s1/WMULTI", "TERH:3001:10:s2/WMULTI", "TERH:3001:10:s3/WMULTI"}
	slices.Sort(ids)
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("paths = %v, want %v", ids, want)
	}
	if res.Channels != 3 || res.Excluded != 2 {
		t.Errorf("channels=%d excluded=%d, want 3 and 2 (WTHI-TV, WJAR)", res.Channels, res.Excluded)
	}
	if len(res.NotInFCC) != 1 || res.NotInFCC[0].RFChannel != 7 {
		t.Errorf("NotInFCC = %+v, want PROV RF 7", res.NotInFCC)
	}
	if len(res.NoCallSign) != 1 || res.NoCallSign[0].RFChannel != 9 {
		t.Errorf("NoCallSign = %+v, want PROV RF 9", res.NoCallSign)
	}
	if !slices.Equal(res.UnknownLRFs, []string{"BIRM"}) {
		t.Errorf("UnknownLRFs = %v, want [BIRM]", res.UnknownLRFs)
	}

	// Path geometry for TERH -> WFAR-TV.
	for _, p := range res.Paths {
		if p.Transmitter.CallSign != "WFAR-TV" {
			continue
		}
		d1 := Distance(p.LRF.Lat, p.LRF.Lon, p.Midpoint.Lat, p.Midpoint.Lon)
		d2 := Distance(p.Midpoint.Lat, p.Midpoint.Lon, p.Transmitter.Location.Lat, p.Transmitter.Location.Lon)
		if math.Abs(d1-d2) > 1 || math.Abs((d1+d2)/metresPerMile-p.DistanceMi) > 0.01 {
			t.Errorf("midpoint not halfway: %.1f m vs %.1f m (path %.2f mi)", d1, d2, p.DistanceMi)
		}
		if p.BearingDeg < 0 || p.BearingDeg >= 360 {
			t.Errorf("bearing %v out of range", p.BearingDeg)
		}
		g := p.Group()
		if g.ID != p.ID() || len(g.Points) != 3 || g.Points[0].Label != LabelLRF || g.Points[1].Label != LabelMidpoint || g.Points[2].Label != LabelTransmitter {
			t.Errorf("group = %+v", g)
		}
	}
}

func TestDiscoverSkipsDistantOwnStation(t *testing.T) {
	// Codes swapped: the demods say "TERH" receives KFAR on RF 33, but KFAR is
	// near Providence, about 850 mi away, so it is outside the 200 mi search
	// and must be found through the nationwide lookup.
	f := &fakeES{t: t}
	es := esClient(t, f)
	received := []Received{{LRFCode: "TERH", RFChannel: 33, Tags: []string{"KFAR (OTA)"}, CallSigns: []string{"KFAR"}}}

	res, err := Discover(context.Background(), es, Options{FCCIndex: "fcc-transmitters", Radius: "200mi"}, []lrf.LRF{terh}, received)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 0 || len(res.TooFar) != 1 || res.Excluded != 0 {
		t.Fatalf("paths=%d tooFar=%+v excluded=%d; want the channel skipped", len(res.Paths), res.TooFar, res.Excluded)
	}
	if !strings.Contains(res.TooFar[0].Detail, "possible LRF code or coordinate mismatch") {
		t.Errorf("detail = %q", res.TooFar[0].Detail)
	}
}

func TestBaseCallSign(t *testing.T) {
	for in, want := range map[string]string{"WTHI-TV": "WTHI", "wjar": "WJAR", "WCCU-DT": "WCCU", "KXYZ-LD": "KXYZ"} {
		if got := baseCallSign(in); got != want {
			t.Errorf("baseCallSign(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverExcludesChannelSharingPartner(t *testing.T) {
	f := &fakeES{t: t}
	es := esClient(t, f)
	received := []Received{{LRFCode: "PROV", RFChannel: 2, Tags: []string{"WSBE (OTA)"}, CallSigns: []string{"WSBE"}}}

	res, err := Discover(context.Background(), es, Options{FCCIndex: "fcc-transmitters", Radius: "200mi"}, []lrf.LRF{prov}, received)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, p := range res.Paths {
		got = append(got, p.Transmitter.CallSign)
	}
	slices.Sort(got)
	// WSBE-TV is the own station, WRIW-CD shares its transmitter; WNRB (1.5 km
	// away) and WFAR2 are real co-channel paths.
	if want := []string{"WFAR2", "WNRB"}; !slices.Equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
	if res.Excluded != 1 || res.SharedSite != 1 {
		t.Errorf("excluded=%d shared=%d, want 1 and 1", res.Excluded, res.SharedSite)
	}
}

func TestSharedSiteDistance(t *testing.T) {
	own := []fcc.GeoPoint{{Lat: 41.865389, Lon: -71.286861}}
	tests := []struct {
		name string
		p    fcc.GeoPoint
		maxM float64
		want bool
	}{
		{"same tower", fcc.GeoPoint{Lat: 41.865389, Lon: -71.286861}, 1000, true},
		{"~800 m away", fcc.GeoPoint{Lat: 41.865389, Lon: -71.277200}, 1000, true},
		{"~1.5 km away", fcc.GeoPoint{Lat: 41.865389, Lon: -71.268800}, 1000, false},
		{"~1.5 km, wider limit", fcc.GeoPoint{Lat: 41.865389, Lon: -71.268800}, 2000, true},
	}
	for _, tt := range tests {
		if got := sharesSite(tt.p, own, tt.maxM); got != tt.want {
			t.Errorf("%s: sharesSite = %v, want %v (%.0f m)", tt.name, got, tt.want, Distance(own[0].Lat, own[0].Lon, tt.p.Lat, tt.p.Lon))
		}
	}
}

func ptrF(v float64) *float64 { return &v }

func TestDiscoverNumbersMultiSiteStations(t *testing.T) {
	f := &fakeES{t: t}
	es := esClient(t, f)
	received := []Received{{LRFCode: "TERH", RFChannel: 10, Tags: []string{"WTHI (OTA)"}, CallSigns: []string{"WTHI"}}}

	res, err := Discover(context.Background(), es, Options{FCCIndex: "fcc-transmitters", Radius: "200mi"}, []lrf.LRF{terh}, received)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, p := range res.Paths {
		if _, dup := got[p.ID()]; dup {
			t.Errorf("duplicate path ID %s", p.ID())
		}
		got[p.ID()] = p.Transmitter.Label()
	}
	want := map[string]string{
		"TERH:1001:10":    "WFAR-TV", // single site: ID and label unchanged
		"TERH:3001:10:s1": "WMULTI site 1 of 3 (15 kW)",
		"TERH:3001:10:s2": "WMULTI site 2 of 3 (5 kW)",
		"TERH:3001:10:s3": "WMULTI site 3 of 3 (0.32 kW)",
	}
	if len(got) != len(want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	for id, label := range want {
		if got[id] != label {
			t.Errorf("%s: label %q, want %q", id, got[id], label)
		}
	}
}

func TestNumberSites(t *testing.T) {
	doc := func(fac string, rf int, erp float64, ant string, lat float64) fcc.Document {
		return fcc.Document{FacilityID: fac, RFChannel: rf, ERPkW: &erp, AntennaID: ant, Location: fcc.GeoPoint{Lat: lat, Lon: -70}}
	}
	all := []fcc.Document{
		doc("A", 10, 5, "x2", 40.0),
		doc("A", 10, 5, "x1", 41.0), // same ERP: antenna ID breaks the tie
		doc("A", 10, 20, "x9", 42.0),
		doc("A", 22, 1, "y1", 43.0),  // same facility, other channel (e.g. DRT): its own group
		doc("B", 10, 50, "z1", 44.0), // single site
	}
	n := numberSites(all)
	check := func(d fcc.Document, want site) {
		t.Helper()
		if got := n[siteKey(d)]; got != want {
			t.Errorf("%s ch %d %s: site %v, want %v", d.FacilityID, d.RFChannel, d.AntennaID, got, want)
		}
	}
	check(all[2], site{1, 3})
	check(all[1], site{2, 3})
	check(all[0], site{3, 3})
	check(all[3], site{1, 1})
	check(all[4], site{1, 1})

	// Input order doesn't matter.
	slices.Reverse(all)
	n2 := numberSites(all)
	for k, v := range n {
		if n2[k] != v {
			t.Errorf("numbering depends on input order: %s %v vs %v", k, v, n2[k])
		}
	}
}

func TestTransmitterLabel(t *testing.T) {
	erp := 5.0
	tests := []struct {
		tx   Transmitter
		want string
	}{
		{Transmitter{CallSign: "WJAR", Site: 1, Sites: 1}, "WJAR"},
		{Transmitter{CallSign: "WJAR"}, "WJAR"},
		{Transmitter{CallSign: "WVER", Site: 2, Sites: 4, ERPkW: &erp}, "WVER site 2 of 4 (5 kW)"},
		{Transmitter{CallSign: "WVER", Site: 3, Sites: 4}, "WVER site 3 of 4"},
	}
	for _, tt := range tests {
		if got := tt.tx.Label(); got != tt.want {
			t.Errorf("Label() = %q, want %q", got, tt.want)
		}
	}
}
