package beater

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/lrf"
	"github.com/thetherington/tropobeat/internal/openmeteo/mock"
)

// mockWeatherTag marks weather and ducting events built from mock weather.
const mockWeatherTag = "mock-weather"

// startMockWeather serves simulated Open-Meteo weather on a local port and
// returns the server and the forecast URL to point the weather client at.
// sites receives the per-LRF scenario overrides (see setMockSites).
func startMockWeather(cfg mock.Config, sites *mock.Sites) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("starting mock weather server: %w", err)
	}
	srv := &http.Server{
		Handler:           mock.Handler(cfg, time.Now, sites),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Serve returns when the server is shut down; if it fails earlier, the
	// weather poll's requests fail and are reported there.
	go func() { _ = srv.Serve(ln) }()
	return srv, "http://" + ln.Addr().String() + "/v1/forecast", nil
}

// setMockSites gives the mock the points of every co-channel path of the
// LRFs in openmeteo.mock.sites, with their scenario. The handler only sees
// coordinates, so it is called after each discovery run with the new paths.
func setMockSites(sites *mock.Sites, cfg map[string]string, lrfs []lrf.LRF, paths []cochannel.Path) {
	if sites == nil || len(cfg) == 0 {
		return
	}
	scenario := map[string]string{}
	for code, sc := range cfg {
		scenario[demod.NormalizeLRF(code)] = sc
	}
	var pts []mock.SitePoint
	for _, l := range lrfs {
		if sc, ok := scenario[demod.NormalizeLRF(l.Code)]; ok {
			pts = append(pts, mock.SitePoint{Lat: l.Latitude, Lon: l.Longitude, Scenario: sc})
		}
	}
	for _, p := range paths {
		sc, ok := scenario[demod.NormalizeLRF(p.LRFCode)]
		if !ok {
			continue
		}
		for _, pt := range []cochannel.Point{p.LRF, p.Midpoint, p.Transmitter.Location} {
			pts = append(pts, mock.SitePoint{Lat: pt.Lat, Lon: pt.Lon, Scenario: sc})
		}
	}
	sites.Set(pts)
}

func stopMockWeather(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// tagMock marks events as built from mock weather.
func tagMock(events []beat.Event) {
	for i := range events {
		events[i].Fields["tags"] = []string{mockWeatherTag}
	}
}
