package beater

import (
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/mapstr"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// Metricset names published by tropobeat.
const (
	metricsetCochannel = "cochannel"
	metricsetWeather   = "weather"
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
func weatherEvent(g openmeteo.GroupProfile, path *cochannel.Path, lrfCode, runID string, at time.Time) beat.Event {
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

	var forecast time.Time
	for _, pt := range g.Points {
		if pt.Profile == nil {
			continue
		}
		w[pt.Label] = profileFields(pt)
		forecast = pt.Profile.Time
	}
	w["forecast_time"] = forecast

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

func geoPoint(p cochannel.Point) mapstr.M {
	return mapstr.M{"lat": p.Lat, "lon": p.Lon}
}
