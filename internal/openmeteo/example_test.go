package openmeteo_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// exampleServer stands in for the Open-Meteo API so the example is
// deterministic and runs offline.
func exampleServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"latitude": 39.746895, "longitude": -104.987076, "elevation": 1597.0,
			"hourly": {
				"time": ["2026-10-07T12:00"],
				"temperature_2m": [11.1], "relative_humidity_2m": [57], "surface_pressure": [838.6],
				"temperature_850hPa": [17.6], "relative_humidity_850hPa": [40], "geopotential_height_850hPa": [1541],
				"temperature_700hPa": [2.4], "relative_humidity_700hPa": [35], "geopotential_height_700hPa": [3189]
			}
		}`))
	}))
}

func ExampleClient_Profiles() {
	srv := exampleServer()
	defer srv.Close()

	// In tropobeat the Config comes from the openmeteo section of
	// tropobeat.yml. URL is normally left empty to use the public API (or the
	// customer API when APIKey is set).
	cfg := openmeteo.DefaultConfig
	cfg.URL = srv.URL
	cfg.PressureLevels = []int{850, 700}

	client, err := openmeteo.New(cfg)
	if err != nil {
		panic(err)
	}

	coords := []openmeteo.Coordinate{{Lat: 39.74, Lon: -104.99}} // Denver
	at := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)

	profiles, err := client.Profiles(context.Background(), coords, at)
	if err != nil {
		// Profiles still holds every coordinate that succeeded.
		var batchErr *openmeteo.BatchError
		var apiErr *openmeteo.APIError
		switch {
		case errors.As(err, &apiErr):
			fmt.Println("Open-Meteo rejected the request:", apiErr.Reason)
		case errors.As(err, &batchErr):
			fmt.Printf("%d coordinates failed: %v\n", len(batchErr.Coordinates), batchErr.Err)
		default:
			fmt.Println("error:", err)
		}
	}

	for _, p := range profiles {
		fmt.Printf("%v elevation=%.0fm %s\n", p.Requested, p.Elevation, p.Time.Format(time.RFC3339))
		fmt.Printf("  surface %6.1f hPa %5.0f m %6.2f K %3.0f %%\n",
			*p.Surface.PressureHPa, *p.Surface.HeightM, *p.Surface.TemperatureK, *p.Surface.RelativeHumidityPct)
		for _, l := range p.Levels {
			fmt.Printf("  level   %6.1f hPa %5.0f m %6.2f K %3.0f %%\n",
				*l.PressureHPa, *l.HeightM, *l.TemperatureK, *l.RelativeHumidityPct)
		}
	}

	// Output:
	// 39.74,-104.99 elevation=1597m 2026-10-07T12:00:00Z
	//   surface  838.6 hPa  1599 m 284.25 K  57 %
	//   level    850.0 hPa  1541 m 290.75 K  40 %
	//   level    700.0 hPa  3189 m 275.55 K  35 %
}

func ExampleClient_Hourly() {
	srv := exampleServer()
	defer srv.Close()

	client, err := openmeteo.New(openmeteo.Config{URL: srv.URL})
	if err != nil {
		panic(err)
	}

	series, err := client.Hourly(context.Background(),
		[]openmeteo.Coordinate{{Lat: 39.74, Lon: -104.99}},
		[]string{"temperature_2m", "surface_pressure"})
	if err != nil {
		panic(err)
	}

	for _, s := range series {
		for i, t := range s.Times {
			fmt.Printf("%s grid=%v temperature_2m=%.1f°C surface_pressure=%.1fhPa\n",
				t.Format(time.RFC3339), s.Grid, *s.Values["temperature_2m"][i], *s.Values["surface_pressure"][i])
		}
	}

	// Output:
	// 2026-10-07T12:00:00Z grid=39.746895,-104.987076 temperature_2m=11.1°C surface_pressure=838.6hPa
}
