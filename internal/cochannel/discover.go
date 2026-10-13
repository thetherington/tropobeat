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
	for _, r := range received {
		l, ok := sites[r.LRFCode]
		if !ok {
			unknown[r.LRFCode] = struct{}{}
			continue
		}
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
	byChannel := map[int][]fcc.Document{}
	for _, d := range all {
		byChannel[d.RFChannel] = append(byChannel[d.RFChannel], d)
	}

	for i, r := range todo {
		res.Channels++
		l := sites[r.LRFCode]
		paths, note, excluded := channelPaths(l, r, hits[i], byChannel[r.RFChannel], opts.MaxOwnStationMiles)
		switch {
		case note != nil && excluded < 0:
			res.TooFar = append(res.TooFar, *note)
			continue
		case note != nil:
			res.NotInFCC = append(res.NotInFCC, *note)
		}
		res.Excluded += max(excluded, 0)
		res.Paths = append(res.Paths, paths...)
	}

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
// far away).
func channelPaths(l lrf.LRF, r Received, hits, sameChannel []fcc.Document, maxOwnMiles float64) (paths []Path, note *Note, excluded int) {
	nearestOwnM := -1.0
	for _, d := range sameChannel {
		if !slices.Contains(r.CallSigns, baseCallSign(d.CallSign)) {
			continue
		}
		if dist := Distance(l.Latitude, l.Longitude, d.Location.Lat, d.Location.Lon); nearestOwnM < 0 || dist < nearestOwnM {
			nearestOwnM = dist
		}
	}

	switch {
	case nearestOwnM < 0:
		note = &Note{LRFCode: r.LRFCode, RFChannel: r.RFChannel, Tags: r.Tags,
			Detail: fmt.Sprintf("call signs %s not in FCC transmitters on RF %d; nothing excluded", strings.Join(r.CallSigns, "/"), r.RFChannel)}
	case nearestOwnM/metresPerMile > maxOwnMiles:
		return nil, &Note{LRFCode: r.LRFCode, RFChannel: r.RFChannel, Tags: r.Tags,
			Detail: fmt.Sprintf("own station %s is %.0f mi away (limit %.0f); possible LRF code or coordinate mismatch", strings.Join(r.CallSigns, "/"), nearestOwnM/metresPerMile, maxOwnMiles)}, -1
	}

	for _, h := range hits {
		if slices.Contains(r.CallSigns, baseCallSign(h.CallSign)) {
			excluded++
			continue
		}
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
			},
			Midpoint:   Point{Lat: midLat, Lon: midLon},
			DistanceMi: distM / metresPerMile,
			BearingDeg: bearing,
		})
	}
	return paths, note, excluded
}

// channelQuery finds the transmitters on rf within radius of the LRF.
func channelQuery(l lrf.LRF, rf int, radius string) (string, error) {
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

// baseCallSign strips a service suffix such as "-TV" or "-DT".
func baseCallSign(cs string) string {
	base, _, _ := strings.Cut(strings.ToUpper(strings.TrimSpace(cs)), "-")
	return base
}
