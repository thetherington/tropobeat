package elastic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
)

// EnsureIndex creates index with the given settings/mappings body if it does
// not already exist. If recreate is true, an existing index is deleted first.
// It reports whether the index was created.
func EnsureIndex(ctx context.Context, client *elasticsearch.Client, index, body string, recreate bool) (bool, error) {
	res, err := client.Indices.Exists([]string{index}, client.Indices.Exists.WithContext(ctx))
	if err != nil {
		return false, fmt.Errorf("checking index %s: %w", index, err)
	}
	res.Body.Close()

	switch res.StatusCode {
	case 200:
		if !recreate {
			return false, nil
		}
		res, err := client.Indices.Delete([]string{index}, client.Indices.Delete.WithContext(ctx))
		if err != nil {
			return false, fmt.Errorf("deleting index %s: %w", index, err)
		}
		defer res.Body.Close()
		if res.IsError() {
			return false, fmt.Errorf("deleting index %s: %s", index, res.String())
		}
	case 404:
	default:
		return false, fmt.Errorf("checking index %s: unexpected status %s", index, res.Status())
	}

	res, err = client.Indices.Create(index,
		client.Indices.Create.WithContext(ctx),
		client.Indices.Create.WithBody(strings.NewReader(body)),
	)
	if err != nil {
		return false, fmt.Errorf("creating index %s: %w", index, err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return false, fmt.Errorf("creating index %s: %s", index, res.String())
	}

	return true, nil
}

// Doc is a document to bulk index. An empty ID lets Elasticsearch generate one.
type Doc struct {
	ID     string
	Source any
}

// BulkStats summarizes a BulkIndex run.
type BulkStats struct {
	Indexed uint64
	Failed  uint64
}

// BulkIndex indexes docs into index and refreshes it so they are immediately
// searchable. Documents with an existing ID are overwritten. Per-document
// failures are collected and returned as a single error alongside the stats.
func BulkIndex(ctx context.Context, client *elasticsearch.Client, index string, docs []Doc) (BulkStats, error) {
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Client: client,
		Index:  index,
	})
	if err != nil {
		return BulkStats{}, err
	}

	var (
		stats    BulkStats
		failures = make(chan string, len(docs))
	)

	for _, d := range docs {
		body, err := json.Marshal(d.Source)
		if err != nil {
			return stats, fmt.Errorf("encoding document %q: %w", d.ID, err)
		}

		err = bi.Add(ctx, esutil.BulkIndexerItem{
			Action:     "index",
			DocumentID: d.ID,
			Body:       bytes.NewReader(body),
			OnSuccess: func(context.Context, esutil.BulkIndexerItem, esutil.BulkIndexerResponseItem) {
				atomic.AddUint64(&stats.Indexed, 1)
			},
			OnFailure: func(_ context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
				atomic.AddUint64(&stats.Failed, 1)
				if err == nil {
					err = fmt.Errorf("%s: %s", res.Error.Type, res.Error.Reason)
				}
				failures <- fmt.Sprintf("document %q: %v", item.DocumentID, err)
			},
		})
		if err != nil {
			return stats, fmt.Errorf("queueing document %q: %w", d.ID, err)
		}
	}

	if err := bi.Close(ctx); err != nil {
		return stats, fmt.Errorf("bulk indexing: %w", err)
	}
	close(failures)

	res, err := client.Indices.Refresh(client.Indices.Refresh.WithIndex(index), client.Indices.Refresh.WithContext(ctx))
	if err != nil {
		return stats, fmt.Errorf("refreshing index %s: %w", index, err)
	}
	res.Body.Close()

	if stats.Failed > 0 {
		var msgs []string
		for m := range failures {
			msgs = append(msgs, m)
		}
		return stats, fmt.Errorf("%d document(s) failed:\n  %s", stats.Failed, strings.Join(msgs, "\n  "))
	}

	return stats, nil
}

// DeleteByQuery deletes the documents in index matching query (a JSON
// query clause, e.g. {"range":{...}}) and returns how many were deleted.
func DeleteByQuery(ctx context.Context, client *elasticsearch.Client, index, query string) (int64, error) {
	res, err := client.DeleteByQuery([]string{index},
		strings.NewReader(`{"query":`+query+`}`),
		client.DeleteByQuery.WithContext(ctx),
		client.DeleteByQuery.WithRefresh(true),
		client.DeleteByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		return 0, fmt.Errorf("deleting from %s: %w", index, err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return 0, fmt.Errorf("deleting from %s: %s", index, res.String())
	}

	var body struct {
		Deleted  int64             `json:"deleted"`
		Failures []json.RawMessage `json:"failures"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decoding delete response from %s: %w", index, err)
	}
	if len(body.Failures) > 0 {
		return body.Deleted, fmt.Errorf("deleting from %s: %d failures, first: %s", index, len(body.Failures), body.Failures[0])
	}
	return body.Deleted, nil
}
