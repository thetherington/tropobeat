package elastic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/elastic/go-elasticsearch/v8"
)

// maxResultWindow is Elasticsearch's default index.max_result_window, the
// most documents a single search can return.
const maxResultWindow = 10000

// SearchAll returns the _source of every document in index, decoded as T.
// It is meant for small reference indices: it fails rather than returning a
// partial result if the index holds more than 10,000 documents.
func SearchAll[T any](ctx context.Context, client *elasticsearch.Client, index string) ([]T, error) {
	res, err := client.Search(
		client.Search.WithContext(ctx),
		client.Search.WithIndex(index),
		client.Search.WithBody(strings.NewReader(`{"query":{"match_all":{}}}`)),
		client.Search.WithSize(maxResultWindow),
		client.Search.WithTrackTotalHits(true),
	)
	if err != nil {
		return nil, fmt.Errorf("searching %s: %w", index, err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return nil, fmt.Errorf("searching %s: %s", index, res.String())
	}

	var body struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source T `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding search response from %s: %w", index, err)
	}
	if total := body.Hits.Total.Value; total > len(body.Hits.Hits) {
		return nil, fmt.Errorf("index %s has %d documents; SearchAll supports at most %d", index, total, maxResultWindow)
	}

	docs := make([]T, len(body.Hits.Hits))
	for i, h := range body.Hits.Hits {
		docs[i] = h.Source
	}
	return docs, nil
}

// msearchChunk is the number of searches sent per _msearch request.
const msearchChunk = 100

// MultiSearch runs each query (a full search body, e.g. {"size":..,"query":..})
// against index and returns the _source of each query's hits, decoded as T,
// in the same order as queries. Queries are sent in chunks of 100 per
// _msearch request. Any failed query fails the whole call.
func MultiSearch[T any](ctx context.Context, client *elasticsearch.Client, index string, queries []string) ([][]T, error) {
	header, err := json.Marshal(map[string]string{"index": index})
	if err != nil {
		return nil, err
	}

	results := make([][]T, 0, len(queries))
	for start := 0; start < len(queries); start += msearchChunk {
		chunk := queries[start:min(start+msearchChunk, len(queries))]

		var body strings.Builder
		for _, q := range chunk {
			body.Write(header)
			body.WriteByte('\n')
			body.WriteString(q)
			body.WriteByte('\n')
		}

		res, err := client.Msearch(strings.NewReader(body.String()), client.Msearch.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("multi-search on %s: %w", index, err)
		}
		chunkResults, err := decodeMultiSearch[T](res.Body, len(chunk))
		res.Body.Close()
		if err == nil && res.IsError() {
			err = fmt.Errorf("HTTP %d", res.StatusCode)
		}
		if err != nil {
			return nil, fmt.Errorf("multi-search on %s: %w", index, err)
		}
		results = append(results, chunkResults...)
	}
	return results, nil
}

func decodeMultiSearch[T any](r io.Reader, want int) ([][]T, error) {
	var body struct {
		Responses []struct {
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
			Hits   struct {
				Hits []struct {
					Source T `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		} `json:"responses"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if len(body.Responses) != want {
		return nil, fmt.Errorf("got %d responses for %d searches", len(body.Responses), want)
	}

	out := make([][]T, len(body.Responses))
	for i, r := range body.Responses {
		if len(r.Error) > 0 && string(r.Error) != "null" {
			return nil, fmt.Errorf("search %d failed: %s", i, r.Error)
		}
		docs := make([]T, len(r.Hits.Hits))
		for j, h := range r.Hits.Hits {
			docs[j] = h.Source
		}
		out[i] = docs
	}
	return out, nil
}
