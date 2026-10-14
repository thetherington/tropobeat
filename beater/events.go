package beater

import (
	"slices"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/mapstr"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// Metricset names published by tropobeat.
const (
	metricsetCochannel = "cochannel"
	metricsetWeather   = "weather"
	metricsetDucting   = "ducting"
)

// cochannelEvent describes one co-channel path found by a discovery run.
func cochannelEvent(p cochannel.Path, runID string, at time.Time) beat.Event {
	tx := mapstr.M{
		"call_sign":   p.Transmitter.CallSign,
		"facility_id": p.Transmitter.FacilityID,
		"service":     p.Transmitter.Service,
		"location":    geoPoint(p.Transmitter.Location),
	}
	if p.Transmitter.ERPkW != nil {
		tx["erp_kw"] = *p.Transmitter.ERPkW
	}

	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetCochannel},
			"tropobeat": mapstr.M{"cochannel": mapstr.M{
				"run_id":     runID,
				"path_id":    p.ID(),
				"rf_channel": p.RFChannel,
				"lrf": mapstr.M{
					"code":       p.LRFCode,
					"location":   geoPoint(p.LRF),
					"call_signs": p.LRFCallSigns,
				},
				"transmitter": tx,
				"midpoint":    mapstr.M{"location": geoPoint(p.Midpoint)},
				"distance_mi": p.DistanceMi,
				"bearing_deg": p.BearingDeg,
				"path":        pathLine(p),
			}},
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
	w := mapstr.M{
		"path_id":  g.ID,
		"lrf_code": lrfCode,
	}
	if runID != "" {
		w["run_id"] = runID
	}
	if path != nil {
		w["rf_channel"] = path.RFChannel
		w["transmitter_call_sign"] = path.Transmitter.CallSign
		w["path"] = pathLine(*path)
	}

	var (
		forecast time.Time
		analyses []ducting.Analysis
	)
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
		forecast = pt.Profile.Time
	}
	w["forecast_time"] = forecast

	// The path ducting index is the mean of the usable points' scores.
	// trapped needs the channel's frequency, so it is left out if the
	// channel isn't a TV channel.
	if path != nil {
		mhz, known := ducting.ChannelMHz(path.RFChannel)
		if index, trapped, ok := ducting.PathIndex(analyses, mhz); ok {
			d := mapstr.M{"index": index}
			if known {
				d["frequency_mhz"] = mhz
				d["trapped"] = trapped
			}
			w["ducting"] = d
		}
	}

	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetWeather},
			"tropobeat": mapstr.M{"weather": w},
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
// analysis and the paths and LRFs whose weather used it.
type location struct {
	id       string
	coord    openmeteo.Coordinate
	profile  *openmeteo.Profile
	analysis ducting.Analysis
	pathIDs  map[string]struct{}
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
		"forecast_time":  loc.profile.Time,
		"location":       mapstr.M{"lat": loc.coord.Lat, "lon": loc.coord.Lon},
		"elevation_m":    loc.profile.Elevation,
		"class":          string(a.Class),
		"dropped_levels": a.Dropped,
		"path_ids":       sortedKeys(loc.pathIDs),
		"lrf_codes":      sortedKeys(loc.lrfCodes),
	}
	if runID != "" {
		d["run_id"] = runID
	}
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

	return beat.Event{
		Timestamp: at,
		Fields: mapstr.M{
			"metricset": mapstr.M{"name": metricsetDucting},
			"tropobeat": mapstr.M{"ducting": d},
		},
	}
}

func sortedKeys(m map[string]struct{}) []string {
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
