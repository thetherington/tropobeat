package beater

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"

	"github.com/thetherington/tropobeat/internal/openmeteo/mock"
)

// mockWeatherTag marks weather and ducting events built from mock weather.
const mockWeatherTag = "mock-weather"

// startMockWeather serves simulated Open-Meteo weather on a local port and
// returns the server and the forecast URL to point the weather client at.
func startMockWeather(cfg mock.Config) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("starting mock weather server: %w", err)
	}
	srv := &http.Server{
		Handler:           mock.Handler(cfg, time.Now),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Serve returns when the server is shut down; if it fails earlier, the
	// weather poll's requests fail and are reported there.
	go func() { _ = srv.Serve(ln) }()
	return srv, "http://" + ln.Addr().String() + "/v1/forecast", nil
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
