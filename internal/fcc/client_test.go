package fcc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

type fakeFCC struct {
	body   []byte
	status []int // popped per request; 0 or empty means 200 with body

	mu       sync.Mutex
	requests []*http.Request
	times    []time.Time
}

func (f *fakeFCC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.times = append(f.times, time.Now())
	var status int
	if len(f.status) > 0 {
		status, f.status = f.status[0], f.status[1:]
	}
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	_, _ = w.Write(f.body)
}

func newFake(t *testing.T, status ...int) (*fakeFCC, *Client) {
	t.Helper()
	body, err := os.ReadFile("testdata/tvq_ri.txt")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFCC{body: body, status: status}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	c := NewClient(Config{BaseURL: srv.URL, MaxRetries: 2})
	c.backoff = time.Millisecond
	return f, c
}

func TestFetchRequest(t *testing.T) {
	f, c := newFake(t)

	ts, _, err := c.Fetch(context.Background(), "RI")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 19 {
		t.Errorf("got %d records, want 19", len(ts))
	}

	r := f.requests[0]
	if ua := r.Header.Get("User-Agent"); ua != DefaultUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, DefaultUserAgent)
	}
	q := r.URL.Query()
	if q.Get("state") != "RI" || q.Get("list") != "4" || q.Get("vac") != "0" {
		t.Errorf("query = %v", q)
	}
}

func TestFetchRetriesThrottling(t *testing.T) {
	f, c := newFake(t, http.StatusTooManyRequests, http.StatusServiceUnavailable)

	ts, _, err := c.Fetch(context.Background(), "RI")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 19 || len(f.requests) != 3 {
		t.Errorf("got %d records after %d requests, want 19 after 3", len(ts), len(f.requests))
	}
}

func TestFetchGivesUpAfterMaxRetries(t *testing.T) {
	f, c := newFake(t, 500, 500, 500, 500)

	if _, _, err := c.Fetch(context.Background(), "RI"); err == nil {
		t.Fatal("expected error")
	}
	if len(f.requests) != 3 {
		t.Errorf("got %d requests, want 3 (1 + 2 retries)", len(f.requests))
	}
}

func TestFetchRefusedNotRetried(t *testing.T) {
	f, c := newFake(t, http.StatusForbidden)

	_, _, err := c.Fetch(context.Background(), "RI")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if len(f.requests) != 1 {
		t.Errorf("got %d requests, want 1", len(f.requests))
	}
}

func TestFetchOtherClientErrorNotRetried(t *testing.T) {
	f, c := newFake(t, http.StatusNotFound)

	if _, _, err := c.Fetch(context.Background(), "RI"); err == nil {
		t.Fatal("expected error")
	}
	if len(f.requests) != 1 {
		t.Errorf("got %d requests, want 1", len(f.requests))
	}
}

func TestFetchPacesRequests(t *testing.T) {
	f, c := newFake(t)
	c.cfg.Delay = 100 * time.Millisecond

	for _, st := range []string{"RI", "CT", "MA"} {
		if _, _, err := c.Fetch(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(f.times); i++ {
		if gap := f.times[i].Sub(f.times[i-1]); gap < 90*time.Millisecond {
			t.Errorf("gap between requests %d and %d = %v, want >= 100ms", i-1, i, gap)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"0", 0},
		{"junk", 0},
		{"86400", maxRetryAfter},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
