package elastic

import (
	"context"
	"encoding/json"
	"fmt"
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
