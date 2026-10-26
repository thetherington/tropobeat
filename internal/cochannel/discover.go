package cochannel

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/elastic/go-elasticsearch/v8"

	"github.com/thetherington/tropobeat/internal/elastic"
	"github.com/thetherington/tropobeat/internal/fcc"
	"github.com/thetherington/tropobeat/internal/lrf"
)

// DefaultMaxOwnStationMiles is the default Options.MaxOwnStationMiles.
const DefaultMaxOwnStationMiles = 150

// DefaultSharedSiteMetres is the default Options.SharedSiteMetres.
const DefaultSharedSiteMetres = 1000

// maxHitsPerChannel bounds the transmitters returned per (LRF, channel)
// search; a 200-mile radius holds far fewer on one channel.
const maxHitsPerChannel = 1000

// Options configures Discover.
type Options struct {
	// FCCIndex is the index holding fcc.Document transmitters.
	FCCIndex string
	// Radius is the search radius as an Elasticsearch distance, e.g. "200mi".
	Radius string
	// MaxOwnStationMiles is how far the LRF's own station may be from the
	// LRF. A matched own station further away than this suggests an LRF
	// code or coordinate mismatch, and the channel is skipped.
	MaxOwnStationMiles float64
	// SharedSiteMetres is how close a transmitter on the same channel must
	// be to one of the LRF's own-station sites to count as a channel-sharing
	// partner (two stations broadcasting from one transmitter) rather than
	// an interferer.
	SharedSiteMetres float64
}

// Note records an (LRF, channel) that needs attention.
type Note struct {
	LRFCode   string
	RFChannel int
	Tags      []string
	Detail    string
}

// Result is the outcome of a discovery run.
type Result struct {
	// Paths are the co-channel paths, sorted by LRF, channel and distance.
	Paths []Path
	// Channels is the number of (LRF, channel) pairs searched.
	Channels int
	// Excluded is the number of transmitters excluded as LRFs' own stations.
	Excluded int
	// SharedSite is the number of transmitters excluded because they share
	// the own station's site and channel (channel sharing): the same signal,
	// not interference.
	SharedSite int
	// NotInFCC lists channels whose tagged call signs matched no
	// transmitter in the index (LPTV, Class A, translators, foreign
	// stations). Nothing is excluded for them.
	NotInFCC []Note
	// NoCallSign lists channels skipped because no tag names a station
	// (e.g. "Spare", "NA").
	NoCallSign []Note
	// TooFar lists channels skipped because the matched own station is
	// further from the LRF than Options.MaxOwnStationMiles.
	TooFar []Note
	// UnknownLRFs lists demod LRF IDs not found in the LRF list.
	UnknownLRFs []string
}

// Discover finds the co-channel paths for the received channels of the
// given LRFs. Received channels for LRFs not in lrfs are reported in
// Result.UnknownLRFs and skipped.
func Discover(ctx context.Context, es *elasticsearch.Client, opts Options, lrfs []lrf.LRF, received []Received) (Result, error) {
	if opts.MaxOwnStationMiles <= 0 {
		opts.MaxOwnStationMiles = DefaultMaxOwnStationMiles
	}
	if opts.SharedSiteMetres <= 0 {
		opts.SharedSiteMetres = DefaultSharedSiteMetres
	}

	// Index the LRFs by upper-cased code, to match the demod IDs, which
	// LoadReceived upper-cases.
	sites := make(map[string]lrf.LRF, len(lrfs))
	for _, l := range lrfs {
		sites[strings.ToUpper(l.Code)] = l
	}

	var (
		res     Result
		unknown = map[string]struct{}{}
		todo    []Received
		queries []string
	)
	// Pass 1: decide which received channels to search. Channels of unknown
	// LRFs and channels with no call sign are set aside here. Each of the
	// rest gets one search query; todo[i] and queries[i] stay aligned so
	// the results can be matched back to their channel below.
	for _, r := range received {
		l, ok := sites[r.LRFCode]
		if !ok {
			unknown[r.LRFCode] = struct{}{}
			continue
		}
		// Without a call sign the LRF's own station can't be identified,
		// so every transmitter on the channel would look like an
		// interferer. These are spare or test inputs, so skip them.
		if len(r.CallSigns) == 0 {
			res.NoCallSign = append(res.NoCallSign, Note{LRFCode: r.LRFCode, RFChannel: r.RFChannel, Tags: r.Tags, Detail: "no call sign in input tag"})
			continue
		}
		q, err := channelQuery(l, r.RFChannel, opts.Radius)
		if err != nil {
			return res, err
		}
		todo = append(todo, r)
		queries = append(queries, q)
	}
	for id := range unknown {
		res.UnknownLRFs = append(res.UnknownLRFs, id)
	}
	slices.Sort(res.UnknownLRFs)

	// Run every channel's search in bulk (100 per request). hits[i] holds
	// the transmitters found for todo[i].
	hits, err := elastic.MultiSearch[fcc.Document](ctx, es, opts.FCCIndex, queries)
	if err != nil {
		return res, err
	}

	// The own station is looked up nationwide, not only within the search
	// radius, so an LRF whose code or coordinates are wrong (its own station
	// hundreds of miles away) is detected rather than given bogus paths.
	all, err := elastic.SearchAll[fcc.Document](ctx, es, opts.FCCIndex)
	if err != nil {
		return res, err
	}
	// Group the full FCC list by RF channel, so each channel's own-station
	// lookup only scans transmitters on that channel. The list is small
	// (about 1,750 full-power stations), so one read per run is cheap.
	byChannel := map[int][]fcc.Document{}
	for _, d := range all {
		byChannel[d.RFChannel] = append(byChannel[d.RFChannel], d)
	}
	// Number the sites of stations with several transmitters on a channel,
	// so each site gets its own path ID and a readable label.
	siteNums := numberSites(all)

	// Pass 2: turn each channel's search hits into paths, and record any
	// channel that needs attention.
	for i, r := range todo {
		res.Channels++
		l := sites[r.LRFCode]
		paths, note, excluded, shared := channelPaths(l, r, hits[i], byChannel[r.RFChannel], siteNums, opts)
		// A note with excluded == -1 means the channel was skipped (own
		// station too far away), so it has no paths. A note otherwise means
		// the own station wasn't in the FCC data: the paths are kept and the
		// channel is just reported.
		switch {
		case note != nil && excluded < 0:
			res.TooFar = append(res.TooFar, *note)
			continue
		case note != nil:
			res.NotInFCC = append(res.NotInFCC, *note)
		}
		res.Excluded += max(excluded, 0)
		res.SharedSite += shared
		res.Paths = append(res.Paths, paths...)
	}

	// Sort so the output is stable from run to run: by LRF, channel and
	// nearest transmitter first. Facility ID breaks ties.
	slices.SortFunc(res.Paths, func(a, b Path) int {
		return cmp.Or(
			cmp.Compare(a.LRFCode, b.LRFCode),
			cmp.Compare(a.RFChannel, b.RFChannel),
			cmp.Compare(a.DistanceMi, b.DistanceMi),
			cmp.Compare(a.Transmitter.FacilityID, b.Transmitter.FacilityID),
		)
	})
	return res, nil
}

// channelPaths applies the own-station exclusion to the transmitters found
// within the radius for one (LRF, channel). sameChannel holds every
// transmitter on the channel nationwide, used to find the own station even
// when it is outside the radius. excluded is the number of own-station
// transmitters removed, or -1 when the channel is skipped (own station too
// far away). shared is the number of transmitters removed because they
// share an own-station site (channel sharing).
func channelPaths(l lrf.LRF, r Received, hits, sameChannel []fcc.Document, siteNums map[string]site, opts Options) (paths []Path, note *Note, excluded, shared int) {
	// Find the LRF's own station: the nearest transmitter on this channel,
	// anywhere in the country, whose call sign is in the input's tag. A
	// station can have several transmitters with the same call sign, so
	// the nearest one is used. -1 means none was found. Every own-station
	// site is kept for the channel-sharing check below.
	nearestOwnM := -1.0
	var ownSites []fcc.GeoPoint
	for _, d := range sameChannel {
		if !slices.Contains(r.CallSigns, baseCallSign(d.CallSign)) {
			continue
		}
		ownSites = append(ownSites, d.Location)
		if dist := Distance(l.Latitude, l.Longitude, d.Location.Lat, d.Location.Lon); nearestOwnM < 0 || dist < nearestOwnM {
			nearestOwnM = dist
		}
	}
	maxOwnMiles := opts.MaxOwnStationMiles

	switch {
	// No own station in the FCC data (LPTV, Class A, translator or a
	// foreign station). Every hit is then a genuine full-power
	// transmitter, so nothing is excluded; the channel is just reported.
	case nearestOwnM < 0:
		note = &Note{LRFCode: r.LRFCode, RFChannel: r.RFChannel, Tags: r.Tags,
			Detail: fmt.Sprintf("call signs %s not in FCC transmitters on RF %d; nothing excluded", strings.Join(r.CallSigns, "/"), r.RFChannel)}
	// The own station is implausibly far away, which points to a wrong LRF
	// code or coordinates (e.g. the MON/MONR swap). Searching around the
	// wrong site would invent paths, so skip the channel.
	case nearestOwnM/metresPerMile > maxOwnMiles:
		return nil, &Note{LRFCode: r.LRFCode, RFChannel: r.RFChannel, Tags: r.Tags,
			Detail: fmt.Sprintf("own station %s is %.0f mi away (limit %.0f); possible LRF code or coordinate mismatch", strings.Join(r.CallSigns, "/"), nearestOwnM/metresPerMile, maxOwnMiles)}, -1, 0
	}

	// Every hit within the radius is a co-channel path, except the LRF's
	// own station(s) and any station sharing an own-station site on this
	// channel, which are counted and dropped. Channel-sharing stations (for
	// example WSBE-TV and WRIW-CD on RF 2 at Providence) broadcast from the
	// same transmitter, so they carry the LRF's own signal, not
	// interference, but the tag names only one of them.
	for _, h := range hits {
		if slices.Contains(r.CallSigns, baseCallSign(h.CallSign)) {
			excluded++
			continue
		}
		if sharesSite(h.Location, ownSites, opts.SharedSiteMetres) {
			shared++
			continue
		}
		// Midpoint, distance and bearing on the WGS84 ellipsoid. The
		// midpoint is where the path's weather is sampled.
		loc := h.Location
		midLat, midLon, distM, bearing := Midpoint(l.Latitude, l.Longitude, loc.Lat, loc.Lon)
		paths = append(paths, Path{
			LRFCode:      r.LRFCode,
			LRF:          Point{Lat: l.Latitude, Lon: l.Longitude},
			LRFCallSigns: r.CallSigns,
			RFChannel:    r.RFChannel,
			Transmitter: Transmitter{
				CallSign:   h.CallSign,
				FacilityID: h.FacilityID,
				Service:    h.Service,
				ERPkW:      h.ERPkW,
				Location:   Point{Lat: loc.Lat, Lon: loc.Lon},
				AntennaID:  h.AntennaID,
				Site:       max(siteNums[siteKey(h)].n, 1),
				Sites:      max(siteNums[siteKey(h)].of, 1),
			},
			Midpoint:   Point{Lat: midLat, Lon: midLon},
			DistanceMi: distM / metresPerMile,
			BearingDeg: bearing,
		})
	}
	return paths, note, excluded, shared
}

// sharesSite reports whether p is within maxM metres of any of sites.
func sharesSite(p fcc.GeoPoint, sites []fcc.GeoPoint, maxM float64) bool {
	for _, s := range sites {
		if Distance(p.Lat, p.Lon, s.Lat, s.Lon) <= maxM {
			return true
		}
	}
	return false
}

// channelQuery finds the transmitters on rf within radius of the LRF. For
// TERH on RF 10 with a 200-mile radius it builds:
//
//	{
//	  "size": 1000,
//	  "query": {
//	    "bool": {
//	      "filter": [
//	        { "term": { "rf_channel": 10 } },
//	        {
//	          "geo_distance": {
//	            "distance": "200mi",
//	            "location": { "lat": 39.465533, "lon": -87.4023442 }
//	          }
//	        }
//	      ]
//	    }
//	  }
//	}
func channelQuery(l lrf.LRF, rf int, radius string) (string, error) {
	// Both clauses are filters (no scoring needed): same RF channel, and
	// within radius of the LRF's location.
	q := map[string]any{
		"size": maxHitsPerChannel,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"rf_channel": rf}},
			map[string]any{"geo_distance": map[string]any{
				"distance": radius,
				"location": map[string]any{"lat": l.Latitude, "lon": l.Longitude},
			}},
		}}},
	}
	b, err := json.Marshal(q)
	return string(b), err
}

// baseCallSign strips a service suffix such as "-TV" or "-DT", so FCC call
// signs ("WTHI-TV") compare equal to the bare call signs parsed from demod
// tags ("WTHI").
func baseCallSign(cs string) string {
	base, _, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(cs)), "-")
	return base
}
