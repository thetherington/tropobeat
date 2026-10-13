package cochannel

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
)

// Demod telemetry fields (rfdemod.rf_demod), all keyword or integer.
const (
	fieldTimestamp   = "@timestamp"
	fieldLRFID       = "rfdemod.rf_demod.s_lrf_id"
	fieldRFChannel   = "rfdemod.rf_demod.i_rf_channel"
	fieldInputTag    = "rfdemod.rf_demod.s_input_tag"
	fieldInputStatus = "rfdemod.rf_demod.s_input_status"

	statusLocked  = "Locked"
	compositeSize = 1000
)

// DefaultDemodIndex is the default index pattern for demod telemetry, which
// is stored in daily indices such as log-metric-p-rfdemod-2026.10.13.
const DefaultDemodIndex = "log-metric-p-rfdemod-*"

// Received is one RF channel an LRF receives, according to its demods.
type Received struct {
	// LRFCode is the demod's s_lrf_id, upper-cased.
	LRFCode   string
	RFChannel int
	// Tags are the distinct input tags seen on this channel, with
	// whitespace normalised.
	Tags []string
	// CallSigns are the call signs parsed from Tags, sorted. Empty when no
	// tag names a station (e.g. "Spare").
	CallSigns []string
}

// LoadReceived returns, per LRF and RF channel, the input tags and call
// signs of locked demod inputs seen in index within the last window.
func LoadReceived(ctx context.Context, es *elasticsearch.Client, index string, window time.Duration) ([]Received, error) {
	type key struct {
		lrf string
		rf  int
	}
	tags := map[key]map[string]struct{}{}

	var after json.RawMessage
	for {
		body, err := receivedQuery(window, after)
		if err != nil {
			return nil, err
		}
		res, err := es.Search(
			es.Search.WithContext(ctx),
			es.Search.WithIndex(index),
			es.Search.WithBody(strings.NewReader(body)),
		)
		if err != nil {
			return nil, fmt.Errorf("querying demod telemetry in %s: %w", index, err)
		}

		var page struct {
			Aggregations struct {
				Pairs struct {
					AfterKey json.RawMessage `json:"after_key"`
					Buckets  []struct {
						Key struct {
							LRF *string `json:"lrf"`
							RF  *int    `json:"rf"`
							Tag *string `json:"tag"`
						} `json:"key"`
					} `json:"buckets"`
				} `json:"pairs"`
			} `json:"aggregations"`
		}
		decodeErr := json.NewDecoder(res.Body).Decode(&page)
		isErr, status := res.IsError(), res.String()
		res.Body.Close()
		if isErr {
			return nil, fmt.Errorf("querying demod telemetry in %s: %s", index, status)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding demod telemetry from %s: %w", index, decodeErr)
		}

		buckets := page.Aggregations.Pairs.Buckets
		for _, b := range buckets {
			if b.Key.LRF == nil || *b.Key.LRF == "" || b.Key.RF == nil {
				continue
			}
			k := key{lrf: strings.ToUpper(strings.TrimSpace(*b.Key.LRF)), rf: *b.Key.RF}
			if tags[k] == nil {
				tags[k] = map[string]struct{}{}
			}
			tag := ""
			if b.Key.Tag != nil {
				tag = NormalizeTag(*b.Key.Tag)
			}
			tags[k][tag] = struct{}{}
		}

		// Composite aggregations page by after_key until a page is empty.
		if len(buckets) == 0 || len(page.Aggregations.Pairs.AfterKey) == 0 {
			break
		}
		after = page.Aggregations.Pairs.AfterKey
	}

	out := make([]Received, 0, len(tags))
	for k, set := range tags {
		r := Received{LRFCode: k.lrf, RFChannel: k.rf}
		calls := map[string]struct{}{}
		for tag := range set {
			if tag != "" {
				r.Tags = append(r.Tags, tag)
			}
			for _, c := range ParseCallSigns(tag) {
				calls[c] = struct{}{}
			}
		}
		for c := range calls {
			r.CallSigns = append(r.CallSigns, c)
		}
		slices.Sort(r.Tags)
		slices.Sort(r.CallSigns)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Received) int {
		if c := strings.Compare(a.LRFCode, b.LRFCode); c != 0 {
			return c
		}
		return a.RFChannel - b.RFChannel
	})
	return out, nil
}

func receivedQuery(window time.Duration, after json.RawMessage) (string, error) {
	composite := map[string]any{
		"size": compositeSize,
		"sources": []any{
			map[string]any{"lrf": map[string]any{"terms": map[string]any{"field": fieldLRFID}}},
			map[string]any{"rf": map[string]any{"terms": map[string]any{"field": fieldRFChannel}}},
			map[string]any{"tag": map[string]any{"terms": map[string]any{"field": fieldInputTag, "missing_bucket": true}}},
		},
	}
	if len(after) > 0 {
		composite["after"] = after
	}
	q := map[string]any{
		"size": 0,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"range": map[string]any{fieldTimestamp: map[string]any{"gte": fmt.Sprintf("now-%ds", int(window.Seconds()))}}},
			map[string]any{"term": map[string]any{fieldInputStatus: statusLocked}},
		}}},
		"aggs": map[string]any{"pairs": map[string]any{"composite": composite}},
	}
	b, err := json.Marshal(q)
	return string(b), err
}

var (
	callSignRe  = regexp.MustCompile(`^[KWX][A-Z0-9]{2,5}$`)
	tagSplitRe  = regexp.MustCompile(`[\s/()]+`)
	tagSpacesRe = regexp.MustCompile(`\s+`)
)

// NormalizeTag trims a demod input tag and collapses runs of whitespace,
// so "KOLR ( OTA )" and "KOLR (  OTA )" compare equal.
func NormalizeTag(tag string) string {
	return tagSpacesRe.ReplaceAllString(strings.TrimSpace(tag), " ")
}

// ParseCallSigns returns every US (K, W) or Mexican (X) call sign in a demod
// input tag, e.g. "WTHI / WTHI2 (OTA)" → [WTHI WTHI2], "WMTW(OTA)" → [WMTW]
// and "XHAS (OTA)" → [XHAS]. Tags that name no station, such as "Spare" or
// "NA", return nil.
func ParseCallSigns(tag string) []string {
	var out []string
	for _, tok := range tagSplitRe.Split(strings.ToUpper(tag), -1) {
		if callSignRe.MatchString(tok) && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}
