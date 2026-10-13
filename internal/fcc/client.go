package fcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// DefaultURL is the FCC TV Query endpoint.
const DefaultURL = "https://transition.fcc.gov/fcc-bin/tvq"

// DefaultUserAgent identifies tropobeat to the FCC. Requests are never
// disguised as a browser.
const DefaultUserAgent = "tropobeat (FCC TV facility sync; +https://github.com/thetherington/tropobeat)"

// maxRetryAfter caps how long a Retry-After header can make us wait.
const maxRetryAfter = 5 * time.Minute

// ErrRefused is returned when the FCC refuses a request (HTTP 403). It is
// not retried.
var ErrRefused = errors.New("request refused by FCC (HTTP 403); not retrying")

// States is the default set of jurisdictions: the 50 states, DC and Puerto
// Rico.
var States = []string{
	"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "FL", "GA",
	"HI", "ID", "IL", "IN", "IA", "KS", "KY", "LA", "ME", "MD",
	"MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ",
	"NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC",
	"SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY",
	"DC", "PR",
}

// Config configures a Client.
type Config struct {
	// BaseURL is the TV Query endpoint. Defaults to DefaultURL.
	BaseURL string
	// UserAgent is sent with every request. Defaults to DefaultUserAgent.
	UserAgent string
	// Timeout applies to each request. Defaults to 30s.
	Timeout time.Duration
	// Delay is the minimum time between requests, including retries.
	Delay time.Duration
	// MaxRetries is the number of retries after a 429, 5xx or network error.
	MaxRetries int
}

// DefaultConfig matches the pacing of the original prototype script.
var DefaultConfig = Config{
	BaseURL:    DefaultURL,
	UserAgent:  DefaultUserAgent,
	Timeout:    30 * time.Second,
	Delay:      1500 * time.Millisecond,
	MaxRetries: 3,
}

// Client fetches transmitter listings from the FCC TV Query service.
type Client struct {
	cfg     Config
	http    *http.Client
	backoff time.Duration

	mu   sync.Mutex
	last time.Time
}

// NewClient creates a Client. Zero-valued BaseURL, UserAgent and Timeout
// are replaced with defaults. TLS certificates are always verified.
func NewClient(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultConfig.BaseURL
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultConfig.UserAgent
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultConfig.Timeout
	}
	return &Client{cfg: cfg, http: &http.Client{}, backoff: 2 * time.Second}
}

// Fetch returns every transmitter record listed for state, and the number
// of records skipped because they couldn't be parsed.
func (c *Client) Fetch(ctx context.Context, state string) ([]Transmitter, int, error) {
	q := url.Values{}
	q.Set("state", state)
	for _, k := range []string{"call", "city", "arn", "serv", "facid", "asrn"} {
		q.Set(k, "")
	}
	q.Set("vac", "0")
	q.Set("list", "4")
	reqURL := c.cfg.BaseURL + "?" + q.Encode()

	for attempt := 0; ; attempt++ {
		body, retryAfter, err := c.get(ctx, reqURL)
		if err == nil {
			return ParseTVQ(bytes.NewReader(body))
		}
		if retryAfter < 0 || attempt >= c.cfg.MaxRetries || ctx.Err() != nil {
			return nil, 0, err
		}
		wait := retryAfter
		if wait == 0 {
			wait = c.backoff << attempt
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, 0, err
		}
	}
}

// get performs one paced request. retryAfter is negative if the error must
// not be retried, zero to use the default backoff, or the server-requested
// delay.
func (c *Client) get(ctx context.Context, reqURL string) (body []byte, retryAfter time.Duration, err error) {
	if err := c.pace(ctx); err != nil {
		return nil, -1, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/plain")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()

	body, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("reading response: %w", err)
	}

	switch {
	case res.StatusCode == http.StatusOK:
		return body, 0, nil
	case res.StatusCode == http.StatusForbidden:
		return nil, -1, ErrRefused
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return nil, parseRetryAfter(res.Header.Get("Retry-After"), time.Now()), fmt.Errorf("FCC returned HTTP %d", res.StatusCode)
	default:
		return nil, -1, fmt.Errorf("FCC returned HTTP %d", res.StatusCode)
	}
}

// pace waits until at least Delay has passed since the previous request.
func (c *Client) pace(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.cfg.Delay - time.Since(c.last); !c.last.IsZero() && wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.last = time.Now()
	return nil
}

// parseRetryAfter parses a Retry-After header given in seconds or as an HTTP
// date. It returns 0 if the header is absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	return min(d, maxRetryAfter)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
