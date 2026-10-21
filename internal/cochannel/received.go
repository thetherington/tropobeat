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

	"github.com/thetherington/tropobeat/internal/demod"
)

// Demod telemetry fields used by the received-channel search; see package
// demod for the full list.
const (
	fieldTimestamp   = demod.FieldTimestamp
	fieldLRFID       = demod.FieldLRFID
	fieldRFChannel   = demod.FieldRFChannel
	fieldInputTag    = demod.FieldInputTag
	fieldInputStatus = demod.FieldInputStatus

	statusLocked  = demod.StatusLocked
	compositeSize = 1000
)

// DefaultDemodIndex is the default index pattern for demod telemetry.
const DefaultDemodIndex = demod.DefaultIndex

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
	// Tags seen per (LRF, channel), as a set. The aggregation returns one
	// bucket per (LRF, channel, tag), so several buckets can fold into one
	// key here: two demods on the same channel, or the same tag spelled
	// with different spacing.
	type key struct {
		lrf string
		rf  int
	}
	tags := map[key]map[string]struct{}{}

	// Page through the composite aggregation. after is nil for the first
	// page, then the previous page's after_key.
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

		// Only the aggregation part of the response is decoded. The key
		// fields are pointers because the tag source can return null (an
		// input with no tag; see missing_bucket in receivedQuery).
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
		// Decode before checking the status, then close the body
		// straight away, since this runs in a loop and a deferred
		// close would hold every page's body open until the end.
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
			// A bucket with no LRF ID or channel can't be placed (14 docs
			// in the 2026-10-13 sample had no s_lrf_id), so skip it.
			if b.Key.LRF == nil || *b.Key.LRF == "" || b.Key.RF == nil {
				continue
			}
			// Upper-case the LRF ID, so it matches the LRF codes however
			// the demod was labelled.
			k := key{lrf: strings.ToUpper(strings.TrimSpace(*b.Key.LRF)), rf: *b.Key.RF}
			if tags[k] == nil {
				tags[k] = map[string]struct{}{}
			}
			// A missing tag is stored as "", so the channel is still
			// recorded. It gives no call signs and is left out of Tags.
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

	// Build one Received per (LRF, channel): its distinct non-empty tags,
	// and the call signs parsed from all of them, de-duplicated (a set, as
	// "WTHI" can appear in several tags on the same channel).
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
		// Maps iterate in random order; sort so the output is stable.
		slices.Sort(r.Tags)
		slices.Sort(r.CallSigns)
		out = append(out, r)
	}
	// Sort by LRF, then channel, for the same reason.
	slices.SortFunc(out, func(a, b Received) int {
		if c := strings.Compare(a.LRFCode, b.LRFCode); c != 0 {
			return c
		}
		return a.RFChannel - b.RFChannel
	})
	return out, nil
}

// receivedQuery builds one page of the received-channel search: every
// distinct (LRF, RF channel, input tag) reported by a locked demod input
// within window. No documents are returned ("size": 0), only the composite
// aggregation's buckets. "missing_bucket" keeps inputs with no tag, which
// come back with a null tag. For a 24-hour window, the first page is:
//
//	{
//	  "size": 0,
//	  "query": {
//	    "bool": {
//	      "filter": [
//	        { "range": { "@timestamp": { "gte": "now-86400s" } } },
//	        { "term": { "rfdemod.rf_demod.s_input_status": "Locked" } }
//	      ]
//	    }
//	  },
//	  "aggs": {
//	    "pairs": {
//	      "composite": {
//	        "size": 1000,
//	        "sources": [
//	          { "lrf": { "terms": { "field": "rfdemod.rf_demod.s_lrf_id" } } },
//	          { "rf":  { "terms": { "field": "rfdemod.rf_demod.i_rf_channel" } } },
//	          { "tag": { "terms": { "field": "rfdemod.rf_demod.s_input_tag", "missing_bucket": true } } }
//	        ]
//	      }
//	    }
//	  }
//	}
//
// Each later page adds the previous response's after_key to the composite,
// so it continues where that page ended:
//
//	"after": { "lrf": "TERH", "rf": 10, "tag": "WTHI / WTHI2 (OTA)" }
func receivedQuery(window time.Duration, after json.RawMessage) (string, error) {
	// The composite aggregation returns one bucket per distinct
	// combination of its sources, compositeSize buckets per page.
	composite := map[string]any{
		"size": compositeSize,
		"sources": []any{
			map[string]any{"lrf": map[string]any{"terms": map[string]any{"field": fieldLRFID}}},
			map[string]any{"rf": map[string]any{"terms": map[string]any{"field": fieldRFChannel}}},
			map[string]any{"tag": map[string]any{"terms": map[string]any{"field": fieldInputTag, "missing_bucket": true}}},
		},
	}
	// On later pages, continue after the last bucket of the previous one.
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
	// callSignRe matches a US (K, W) or Mexican (X) call sign: the prefix
	// letter then 2–5 letters or digits. Digits are allowed so labels such
	// as "WTHI2" (a second feed) and LPTV call signs such as "K22JA" match.
	callSignRe = regexp.MustCompile(`^[KWX][A-Z0-9]{2,5}$`)
	// tagSplitRe splits a tag into words on spaces, "/", "(" and ")", so
	// "WMTW(OTA)" splits like "WMTW (OTA)".
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
	// Upper-case, split into words and keep each word that looks like a
	// call sign, once. Words such as "OTA", "CBS" or "Spare" don't match.
	var out []string
	for _, tok := range tagSplitRe.Split(strings.ToUpper(tag), -1) {
		if callSignRe.MatchString(tok) && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}
