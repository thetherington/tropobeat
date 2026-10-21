package beater

import (
	"cmp"
	"slices"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/mapstr"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/correlator"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/lrf"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// Metricset names published by tropobeat.
const (
	metricsetCochannel = "cochannel"
	metricsetWeather   = "weather"
	metricsetDucting   = "ducting"
	metricsetBaseline  = "baseline"
	// metricsetCorrelation is a correlator verdict event and
	// metricsetCorrelationTrace a decision-trace record.
	metricsetCorrelation      = "correlation"
	metricsetCorrelationTrace = "correlation_trace"
)

// cochannelEvent describes one co-channel path found by a discovery run.
func cochannelEvent(p cochannel.Path, runID string, at time.Time) beat.Event {
	tx := mapstr.M{
		"service":  p.Transmitter.Service,
		"location": geoPoint(p.Transmitter.Location),
	}
	if p.Transmitter.ERPkW != nil {
		tx["erp_kw"] = *p.Transmitter.ERPkW
	}

	k := keys{discoveryRunID: runID}
	k.addPath(&p)
	t := k.fields()
	t["cochannel"] = mapstr.M{
		"lrf":         mapstr.M{"location": geoPoint(p.LRF)},
		"transmitter": tx,
		"midpoint":    mapstr.M{"location": geoPoint(p.Midpoint)},
		"distance_mi": p.DistanceMi,
		"bearing_deg": p.BearingDeg,
		"path":        pathLine(p),
	}
	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetCochannel},
			"tropobeat": t,
		},
	}
}

// baselineEvent describes one demod instance's baseline from a baseline run.
// site is the LRF the instance belongs to, or nil when its LRF ID isn't in
// the LRF list (the location is then left out). paths are the co-channel
// paths on the instance's LRF and channel from discovery run discoveryRunID:
// the transmitters that could interfere with it. cc gives the correlator's
// thresholds, published for reference next to the medians.
func baselineEvent(b Baseline, site *lrf.LRF, paths []cochannel.Path, discoveryRunID, runID string, at time.Time, cc correlator.Config) beat.Event {
	l := mapstr.M{}
	if site != nil {
		l["location"] = mapstr.M{"lat": site.Latitude, "lon": site.Longitude}
	}
	window := mapstr.M{}
	if !b.From.IsZero() && !b.To.IsZero() {
		window["start"] = b.From
		window["end"] = b.To
		window["days"] = b.To.Sub(b.From).Hours() / 24
	}
	f := mapstr.M{
		"run_id": runID,
		"instance": mapstr.M{
			"id":      b.Instance.String(),
			"host_ip": b.Instance.HostIP,
			"slot":    b.Instance.Slot,
			"input":   b.Instance.Input,
		},
		"lrf":         l,
		"demod_side":  b.Side,
		"card_label":  b.CardLabel,
		"frame_label": b.FrameLabel,
		"tags":        b.Tags,
		"status":      b.Status,
		"samples":     b.Samples,
		"window":      window,
		"ber": mapstr.M{
			"median":          b.MedianBER,
			"effective":       b.EffectiveBER,
			"spike_threshold": cc.BERFactor * b.EffectiveBER,
		},
		"mer": mapstr.M{
			"median_db":         b.MedianMER,
			"drop_threshold_db": b.MedianMER - cc.MERDropDB,
		},
	}

	k := keys{
		lrfCodes:         []string{b.LRF},
		rfChannels:       []int{b.RFChannel},
		stationCallSigns: b.CallSigns,
	}
	for i := range paths {
		k.addPath(&paths[i])
	}
	if len(paths) > 0 {
		k.discoveryRunID = discoveryRunID
	}
	t := k.fields()
	t["baseline"] = f
	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetBaseline},
			"tropobeat": t,
		},
	}
}

// weatherEvent describes the weather along one path (or at one LRF with no
// paths). path is nil for LRF-only groups. The event is timestamped at, when
// the weather was fetched; forecast_time is the hour the weather applies to.
//
// locs holds the ducting analysis of each weather location, keyed by
// profile ID. Each point with a usable analysis gets a ducting summary that
// links to its ducting event, and a path gets the path ducting index.
func weatherEvent(g openmeteo.GroupProfile, path *cochannel.Path, lrfCode, runID string, at time.Time, locs map[string]*location) beat.Event {
	w := mapstr.M{}
	k := keys{discoveryRunID: runID}
	if path != nil {
		k.addPath(path)
		w["path"] = pathLine(*path)
	} else {
		k.lrfCodes = []string{lrfCode}
	}

	var analyses []ducting.Analysis
	for _, pt := range g.Points {
		if pt.Profile == nil {
			continue
		}
		fields := profileFields(pt)
		if loc := locs[profileID(pt.Profile)]; loc != nil {
			analyses = append(analyses, loc.analysis)
			if loc.analysis.Usable() {
				fields["ducting"] = ductingSummary(loc.id, loc.analysis)
			}
		}
		w[pt.Label] = fields
		k.forecastTime = pt.Profile.Time
	}

	// The path ducting index is the mean of the usable points' scores.
	// trapped needs the channel's frequency, so it is left out if the
	// channel isn't a TV channel; the ducting state is then never trapping.
	var mhz []float64
	if path != nil {
		f, known := ducting.ChannelMHz(path.RFChannel)
		if known {
			mhz = []float64{f}
		}
		if index, trapped, ok := ducting.PathIndex(analyses, f); ok {
			d := mapstr.M{"index": index}
			if known {
				d["frequency_mhz"] = f
				d["trapped"] = trapped
			}
			w["ducting"] = d
		}
	}
	k.state, k.hasState = ducting.StateOf(analyses, mhz)

	t := k.fields()
	t["weather"] = w
	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetWeather},
			"tropobeat": t,
		},
	}
}

func profileFields(pt openmeteo.PointProfile) mapstr.M {
	p := pt.Profile
	levels := make([]mapstr.M, 0, len(p.Levels))
	for _, l := range p.Levels {
		levels = append(levels, levelFields(l))
	}
	return mapstr.M{
		"location":    mapstr.M{"lat": pt.Coordinate.Lat, "lon": pt.Coordinate.Lon},
		"elevation_m": p.Elevation,
		"surface":     levelFields(p.Surface),
		"levels":      levels,
	}
}

func levelFields(l openmeteo.Level) mapstr.M {
	m := mapstr.M{}
	put := func(k string, v *float64) {
		if v != nil {
			m[k] = *v
		}
	}
	put("pressure_hpa", l.PressureHPa)
	put("height_m", l.HeightM)
	put("temperature_k", l.TemperatureK)
	put("relative_humidity_pct", l.RelativeHumidityPct)
	return m
}

// pathLine returns the path as a GeoJSON LineString (LRF, midpoint,
// transmitter), so Kibana Maps can draw it. GeoJSON coordinates are
// [lon, lat].
func pathLine(p cochannel.Path) mapstr.M {
	return mapstr.M{
		"type": "LineString",
		"coordinates": [][]float64{
			{p.LRF.Lon, p.LRF.Lat},
			{p.Midpoint.Lon, p.Midpoint.Lat},
			{p.Transmitter.Location.Lon, p.Transmitter.Location.Lat},
		},
	}
}

// location is one unique weather location in a poll, with its ducting
// analysis and the paths (by path ID) and LRFs whose weather used it.
type location struct {
	id       string
	coord    openmeteo.Coordinate
	profile  *openmeteo.Profile
	analysis ducting.Analysis
	paths    map[string]*cochannel.Path
	lrfCodes map[string]struct{}
}

// profileID identifies a weather location: the coordinate the weather was
// requested for (rounded when coordinate_precision is set), so every point
// that shared the fetch has the same ID.
func profileID(p *openmeteo.Profile) string {
	return p.Requested.String()
}

// ductingSummary is the short ducting result put on a weather event's point.
func ductingSummary(id string, a ducting.Analysis) mapstr.M {
	return mapstr.M{
		"profile_id":        id,
		"score":             a.Score,
		"class":             string(a.Class),
		"min_dndh_n_per_km": a.MinDNdh,
	}
}

// ductingEvent describes the full ducting analysis of one location.
func ductingEvent(loc *location, runID string, at time.Time) beat.Event {
	a := loc.analysis
	d := mapstr.M{
		"profile_id":     loc.id,
		"location":       mapstr.M{"lat": loc.coord.Lat, "lon": loc.coord.Lon},
		"elevation_m":    loc.profile.Elevation,
		"class":          string(a.Class),
		"dropped_levels": a.Dropped,
	}

	// The location's shared fields cover every path and LRF that used it.
	// Its state is trapping when a duct traps the channel of any of those
	// paths.
	k := keys{discoveryRunID: runID, forecastTime: loc.profile.Time}
	var mhz []float64
	for _, id := range sortedKeys(loc.paths) {
		p := loc.paths[id]
		k.addPath(p)
		if f, ok := ducting.ChannelMHz(p.RFChannel); ok {
			mhz = append(mhz, f)
		}
	}
	k.lrfCodes = append(k.lrfCodes, sortedKeys(loc.lrfCodes)...)
	k.state, k.hasState = ducting.StateOf([]ducting.Analysis{a}, mhz)
	// Score and gradient only mean something when there were layers.
	if a.Usable() {
		d["score"] = a.Score
		d["min_dndh_n_per_km"] = a.MinDNdh
	}

	points := make([]mapstr.M, 0, len(a.Points))
	for _, p := range a.Points {
		points = append(points, mapstr.M{
			"pressure_hpa":            p.PressureHPa,
			"height_m":                p.HeightM,
			"height_agl_m":            p.HeightAGLM,
			"temperature_k":           p.TemperatureK,
			"relative_humidity_pct":   p.RelativeHumidityPct,
			"vapour_pressure_hpa":     p.VapourPressureHPa,
			"refractivity_n":          p.N,
			"modified_refractivity_m": p.M,
		})
	}
	layers := make([]mapstr.M, 0, len(a.Layers))
	for _, l := range a.Layers {
		layers = append(layers, mapstr.M{
			"base_agl_m":    l.BaseAGLM,
			"top_agl_m":     l.TopAGLM,
			"dndh_n_per_km": l.DNdh,
			"dmdh_m_per_km": l.DMdh,
			"class":         string(l.Class),
		})
	}
	ducts := make([]mapstr.M, 0, len(a.Ducts))
	for _, x := range a.Ducts {
		ducts = append(ducts, mapstr.M{
			"type":        string(x.Type),
			"base_agl_m":  x.BaseAGLM,
			"top_agl_m":   x.TopAGLM,
			"thickness_m": x.ThicknessM,
			"delta_m":     x.DeltaM,
		})
	}
	d["points"] = points
	d["layers"] = layers
	d["ducts"] = ducts

	t := k.fields()
	t["ducting"] = d
	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetDucting},
			"tropobeat": t,
		},
	}
}

// keys are the fields every metricset shares, published directly under
// tropobeat so one dashboard filter or search works across all of them.
// The lists are always published as arrays, sorted and without duplicates,
// and empty ones are left out.
type keys struct {
	lrfCodes         []string
	pathIDs          []string
	rfChannels       []int
	txCallSigns      []string
	txFacilityIDs    []string
	stationCallSigns []string
	discoveryRunID   string
	forecastTime     time.Time
	state            ducting.State
	hasState         bool
}

// addPath adds a co-channel path's LRF, path ID, channel, transmitter and
// the LRF's own stations on that channel.
func (k *keys) addPath(p *cochannel.Path) {
	k.lrfCodes = append(k.lrfCodes, p.LRFCode)
	k.pathIDs = append(k.pathIDs, p.ID())
	k.rfChannels = append(k.rfChannels, p.RFChannel)
	k.txCallSigns = append(k.txCallSigns, p.Transmitter.CallSign)
	k.txFacilityIDs = append(k.txFacilityIDs, p.Transmitter.FacilityID)
	k.stationCallSigns = append(k.stationCallSigns, p.LRFCallSigns...)
}

func (k keys) fields() mapstr.M {
	m := mapstr.M{}
	putList(m, "lrf_codes", k.lrfCodes)
	putList(m, "path_ids", k.pathIDs)
	putList(m, "rf_channels", k.rfChannels)
	var bands []string
	for _, rf := range k.rfChannels {
		if b, ok := ducting.Band(rf); ok {
			bands = append(bands, b)
		}
	}
	putList(m, "bands", bands)
	putList(m, "transmitter_call_signs", k.txCallSigns)
	putList(m, "transmitter_facility_ids", k.txFacilityIDs)
	putList(m, "station_call_signs", k.stationCallSigns)
	if k.discoveryRunID != "" {
		m["discovery_run_id"] = k.discoveryRunID
	}
	if !k.forecastTime.IsZero() {
		m["forecast_time"] = k.forecastTime
	}
	if k.hasState {
		m["ducting_state"] = string(k.state)
		m["ducting_level"] = k.state.Level()
	}
	return m
}

// putList sets m[key] to vs sorted and without duplicates or empty strings,
// unless nothing is left.
func putList[T cmp.Ordered](m mapstr.M, key string, vs []T) {
	var zero T
	out := make([]T, 0, len(vs))
	for _, v := range vs {
		if v != zero {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > 0 {
		m[key] = out
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func geoPoint(p cochannel.Point) mapstr.M {
	return mapstr.M{"lat": p.Lat, "lon": p.Lon}
}
