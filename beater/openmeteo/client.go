// Package openmeteo queries the Open-Meteo forecast API
// (https://open-meteo.com) for batches of coordinates and organizes the
// results into typed hourly series and atmospheric profiles.
//
// The package only fetches and organizes data. It does not interpret it: in
// particular, pressure levels that Open-Meteo extrapolates below the terrain
// at high-elevation sites are returned unchanged. Ducting calculations,
// including filtering those levels, belong to the consumer of this package.
package openmeteo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// API endpoints. Commercial API keys only work on the customer host.
const (
	FreeURL     = "https://api.open-meteo.com/v1/forecast"
	CustomerURL = "https://customer-api.open-meteo.com/v1/forecast"

	freeHost = "api.open-meteo.com"
)

// Surface variables requested by Profiles.
const (
	varSurfaceTemp     = "temperature_2m"
	varSurfaceRH       = "relative_humidity_2m"
	varSurfacePressure = "surface_pressure"
)

// ErrHourNotFound is reported for a coordinate whose response does not
// contain the requested hour.
var ErrHourNotFound = errors.New("requested hour not in response")

// Client queries Open-Meteo for multiple coordinates.
type Client interface {
	// Hourly fetches the given hourly variables for coords. Duplicate
	// coordinates are removed, and requests are sent in batches. Results are
	// returned in input order for every coordinate that succeeded; failures
	// are reported in the returned error (see BatchError and CoordinateError),
	// so a non-nil error may accompany partial results.
	Hourly(ctx context.Context, coords []Coordinate, vars []string) ([]HourlySeries, error)

	// Profiles returns the surface and pressure-level profile of each
	// coordinate for the UTC hour containing at. Error handling is the same
	// as for Hourly.
	Profiles(ctx context.Context, coords []Coordinate, at time.Time) ([]Profile, error)
}

// Config configures a Client. It can be unpacked from the beat configuration.
type Config struct {
	// APIKey is required only for commercial use. When set and URL is empty,
	// requests go to CustomerURL.
	APIKey string `config:"api_key"`
	// URL overrides the API endpoint, e.g. for a self-hosted Open-Meteo.
	URL string `config:"url"`
	// Models selects weather models (e.g. "gfs_seamless"). Empty uses
	// Open-Meteo's best_match.
	Models []string `config:"models"`
	// PressureLevels are the levels in hPa requested by Profiles.
	PressureLevels []int `config:"pressure_levels"`
	// BatchSize is the number of coordinates per request.
	BatchSize int `config:"batch_size"`
	// ForecastDays is the number of days of hourly data requested.
	ForecastDays int `config:"forecast_days"`
	// Timeout applies to each HTTP request.
	Timeout time.Duration `config:"timeout"`
	// BatchDelay is the pause between batch requests.
	BatchDelay time.Duration `config:"batch_delay"`
	// MaxRetries is the number of retries after a rate-limit (429), server
	// (5xx) or network error.
	MaxRetries int `config:"max_retries"`
}

// DefaultPressureLevels are used when Config.PressureLevels is empty.
var DefaultPressureLevels = []int{1000, 975, 950, 925, 900, 850, 800, 700}

// DefaultConfig is the default client configuration. Empty PressureLevels
// means DefaultPressureLevels; zero BatchSize, ForecastDays and Timeout are
// also replaced with defaults by New.
var DefaultConfig = Config{
	BatchSize:    50,
	ForecastDays: 1,
	Timeout:      10 * time.Second,
	BatchDelay:   100 * time.Millisecond,
	MaxRetries:   2,
}

// Option customizes a client created by New.
type Option func(*client)

// WithHTTPClient sets the HTTP client used for requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *client) { c.http = hc }
}

type client struct {
	cfg          Config
	baseURL      string
	http         *http.Client
	retryBackoff time.Duration
}

// New creates a Client from cfg.
func New(cfg Config, opts ...Option) (Client, error) {
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultConfig.BatchSize
	}
	if cfg.ForecastDays == 0 {
		cfg.ForecastDays = DefaultConfig.ForecastDays
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultConfig.Timeout
	}
	if len(cfg.PressureLevels) == 0 {
		cfg.PressureLevels = slices.Clone(DefaultPressureLevels)
	}

	switch {
	case cfg.BatchSize < 0:
		return nil, errors.New("openmeteo: batch_size must be positive")
	case cfg.ForecastDays < 0:
		return nil, errors.New("openmeteo: forecast_days must be positive")
	case cfg.Timeout < 0, cfg.BatchDelay < 0:
		return nil, errors.New("openmeteo: timeout and batch_delay must not be negative")
	case cfg.MaxRetries < 0:
		return nil, errors.New("openmeteo: max_retries must not be negative")
	}
	for _, l := range cfg.PressureLevels {
		if l <= 0 {
			return nil, fmt.Errorf("openmeteo: invalid pressure level %d", l)
		}
	}

	baseURL := cfg.URL
	if baseURL == "" {
		baseURL = FreeURL
		if cfg.APIKey != "" {
			baseURL = CustomerURL
		}
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("openmeteo: invalid url %q", baseURL)
	}
	if cfg.APIKey != "" && u.Hostname() == freeHost {
		return nil, fmt.Errorf("openmeteo: an api_key requires the customer endpoint (%s), not %s", CustomerURL, freeHost)
	}

	c := &client{
		cfg:          cfg,
		baseURL:      baseURL,
		http:         http.DefaultClient,
		retryBackoff: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func (c *client) Hourly(ctx context.Context, coords []Coordinate, vars []string) ([]HourlySeries, error) {
	if len(vars) == 0 {
		return nil, errors.New("openmeteo: no variables requested")
	}

	unique := dedupe(coords)
	var (
		results []HourlySeries
		errs    []error
	)

	for start := 0; start < len(unique); start += c.cfg.BatchSize {
		if start > 0 {
			if err := sleep(ctx, c.cfg.BatchDelay); err != nil {
				return results, errors.Join(append(errs, err)...)
			}
		}

		batch := unique[start:min(start+c.cfg.BatchSize, len(unique))]
		locs, err := c.fetch(ctx, batch, vars)
		if err == nil && len(locs) != len(batch) {
			err = fmt.Errorf("got %d locations for %d coordinates", len(locs), len(batch))
		}
		if err != nil {
			errs = append(errs, &BatchError{Coordinates: batch, Err: err})
			if ctx.Err() != nil {
				break
			}
			continue
		}

		for i, loc := range locs {
			s, err := loc.series(batch[i], vars)
			if err != nil {
				errs = append(errs, &CoordinateError{Coordinate: batch[i], Err: err})
				continue
			}
			results = append(results, s)
		}
	}

	return results, errors.Join(errs...)
}

func (c *client) Profiles(ctx context.Context, coords []Coordinate, at time.Time) ([]Profile, error) {
	vars := []string{varSurfaceTemp, varSurfaceRH, varSurfacePressure}
	levels := slices.Clone(c.cfg.PressureLevels)
	slices.SortFunc(levels, func(a, b int) int { return b - a }) // descending pressure = increasing height
	levels = slices.Compact(levels)
	for _, l := range levels {
		vars = append(vars, tempVar(l), rhVar(l), heightVar(l))
	}

	series, err := c.Hourly(ctx, coords, vars)
	errs := []error{err}

	target := at.UTC().Truncate(time.Hour)
	profiles := make([]Profile, 0, len(series))
	for _, s := range series {
		i := slices.IndexFunc(s.Times, target.Equal)
		if i < 0 {
			errs = append(errs, &CoordinateError{
				Coordinate: s.Requested,
				Err:        fmt.Errorf("%w: %s", ErrHourNotFound, target.Format(time.RFC3339)),
			})
			continue
		}

		p := Profile{
			Requested: s.Requested,
			Grid:      s.Grid,
			Elevation: s.Elevation,
			Time:      target,
			Surface: Level{
				PressureHPa:         s.Values[varSurfacePressure][i],
				HeightM:             ptr(s.Elevation + 2),
				TemperatureK:        toKelvin(s.Values[varSurfaceTemp][i]),
				RelativeHumidityPct: s.Values[varSurfaceRH][i],
			},
			Levels: make([]Level, len(levels)),
		}
		for j, l := range levels {
			p.Levels[j] = Level{
				PressureHPa:         ptr(float64(l)),
				HeightM:             s.Values[heightVar(l)][i],
				TemperatureK:        toKelvin(s.Values[tempVar(l)][i]),
				RelativeHumidityPct: s.Values[rhVar(l)][i],
			}
		}
		profiles = append(profiles, p)
	}

	return profiles, errors.Join(errs...)
}

// fetch requests vars for one batch of coordinates, retrying transient
// failures.
func (c *client) fetch(ctx context.Context, batch []Coordinate, vars []string) ([]location, error) {
	lats := make([]string, len(batch))
	lons := make([]string, len(batch))
	for i, co := range batch {
		lats[i] = strconv.FormatFloat(co.Lat, 'f', -1, 64)
		lons[i] = strconv.FormatFloat(co.Lon, 'f', -1, 64)
	}

	q := url.Values{}
	q.Set("latitude", strings.Join(lats, ","))
	q.Set("longitude", strings.Join(lons, ","))
	q.Set("hourly", strings.Join(vars, ","))
	q.Set("timezone", "GMT")
	q.Set("forecast_days", strconv.Itoa(c.cfg.ForecastDays))
	if len(c.cfg.Models) > 0 {
		q.Set("models", strings.Join(c.cfg.Models, ","))
	}
	if c.cfg.APIKey != "" {
		q.Set("apikey", c.cfg.APIKey)
	}
	reqURL := c.baseURL + "?" + q.Encode()

	var err error
	for attempt := 0; ; attempt++ {
		var (
			locs      []location
			retryable bool
		)
		locs, retryable, err = c.do(ctx, reqURL)
		if err == nil {
			return locs, nil
		}
		if !retryable || attempt >= c.cfg.MaxRetries || ctx.Err() != nil {
			return nil, err
		}
		if serr := sleep(ctx, c.retryBackoff<<attempt); serr != nil {
			return nil, err
		}
	}
}

// do performs a single request. It reports whether a failure is worth
// retrying.
func (c *client) do(ctx context.Context, reqURL string) ([]location, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, false, c.redact(err)
	}
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, true, c.redact(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, true, fmt.Errorf("reading response: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		apiErr := &APIError{StatusCode: res.StatusCode}
		var er errorResponse
		if decodeErr := json.Unmarshal(body, &er); decodeErr == nil {
			apiErr.Reason = er.Reason
		}
		retryable := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		return nil, retryable, apiErr
	}

	locs, err := decodeLocations(body)
	return locs, false, err
}

// redact removes the API key from errors that include the request URL.
func (c *client) redact(err error) error {
	if c.cfg.APIKey == "" {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = strings.ReplaceAll(ue.URL, url.QueryEscape(c.cfg.APIKey), "REDACTED")
	}
	return err
}

func dedupe(coords []Coordinate) []Coordinate {
	seen := make(map[Coordinate]struct{}, len(coords))
	out := make([]Coordinate, 0, len(coords))
	for _, c := range coords {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func toKelvin(c *float64) *float64 {
	if c == nil {
		return nil
	}
	return ptr(math.Round((*c+273.15)*100) / 100)
}

func ptr(v float64) *float64 { return &v }

func tempVar(level int) string   { return fmt.Sprintf("temperature_%dhPa", level) }
func rhVar(level int) string     { return fmt.Sprintf("relative_humidity_%dhPa", level) }
func heightVar(level int) string { return fmt.Sprintf("geopotential_height_%dhPa", level) }
