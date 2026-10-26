package elastic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeIndexES answers the calls EnsureIndex makes: cluster info, index
// exists (with the given status), create, and put mapping. It records the
// requests it saw.
type fakeIndexES struct {
	exists   int
	calls    []string
	mappings []byte
}

func (f *fakeIndexES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/" {
		_, _ = w.Write([]byte(`{"version":{"number":"8.17.0"}}`))
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodHead:
		w.WriteHeader(f.exists)
	case r.Method == http.MethodPut && r.URL.Path == "/idx/_mapping":
		f.mappings, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
	case r.Method == http.MethodPut && r.URL.Path == "/idx":
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

const testBody = `{"settings":{"index":{"number_of_shards":1}},"mappings":{"dynamic":"strict","properties":{"a":{"type":"keyword"},"new_field":{"type":"keyword"}}}}`

func newFakeClient(t *testing.T, f *fakeIndexES) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureIndexUpdatesExistingMapping(t *testing.T) {
	f := &fakeIndexES{exists: http.StatusOK}
	srv := newFakeClient(t, f)
	client, err := NewClient(Config{Hosts: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	created, err := EnsureIndex(context.Background(), client, "idx", testBody, false)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("existing index reported as created")
	}

	// Only the mappings section is sent, so new fields are added in place.
	var got map[string]any
	if err := json.Unmarshal(f.mappings, &got); err != nil {
		t.Fatalf("mapping body %q: %v", f.mappings, err)
	}
	props, _ := got["properties"].(map[string]any)
	if got["dynamic"] != "strict" || props["new_field"] == nil || got["settings"] != nil {
		t.Errorf("mapping update = %s", f.mappings)
	}
}

func TestEnsureIndexCreatesMissingIndex(t *testing.T) {
	f := &fakeIndexES{exists: http.StatusNotFound}
	srv := newFakeClient(t, f)
	client, err := NewClient(Config{Hosts: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	created, err := EnsureIndex(context.Background(), client, "idx", testBody, false)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	for _, c := range f.calls {
		if c == "PUT /idx/_mapping" {
			t.Error("a new index shouldn't need a mapping update")
		}
	}
}
