package lrf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thetherington/tropobeat/internal/elastic"
)

// fakeES serves the cluster info and _search endpoints with the given hits.
func fakeES(t *testing.T, total int, docs []Document) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/":
			_, _ = w.Write([]byte(`{"version":{"number":"8.17.0"},"tagline":"You Know, for Search"}`))
		case r.URL.Path == "/"+DefaultIndex+"/_search":
			hits := make([]map[string]any, len(docs))
			for i, d := range docs {
				hits[i] = map[string]any{"_id": d.Code, "_source": d}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hits": map[string]any{
					"total": map[string]any{"value": total, "relation": "eq"},
					"hits":  hits,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found","status":404}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoad(t *testing.T) {
	want := []LRF{
		{DMAName: "Abilene TX", Code: "ABL", Street: "409 Baird St", City: "Abilene", State: "TX", ZipCode: "79602", Latitude: 32.4449556, Longitude: -99.7141538},
		{DMAName: "Bangor ME", Code: "BAN", Street: "329 Mount Hope Ave", City: "Bangor", State: "ME", ZipCode: "04401", Latitude: 44.8169088, Longitude: -68.7520044, GPSDMS: `44° 49' 0.87" N, 68° 45' 7.22" W`},
	}
	docs := []Document{want[0].Document(), want[1].Document()}
	srv := fakeES(t, len(docs), docs)

	client, err := elastic.NewClient(elastic.Config{Hosts: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := Load(context.Background(), client, DefaultIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d LRFs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LRF %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestLoadTooManyDocuments(t *testing.T) {
	srv := fakeES(t, 20000, []Document{{Code: "ABL"}})
	client, err := elastic.NewClient(elastic.Config{Hosts: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(context.Background(), client, DefaultIndex)
	if err == nil || !strings.Contains(err.Error(), "20000 documents") {
		t.Fatalf("err = %v, want too-many-documents error", err)
	}
}

func TestLoadMissingIndex(t *testing.T) {
	srv := fakeES(t, 0, nil)
	client, err := elastic.NewClient(elastic.Config{Hosts: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Load(context.Background(), client, "no-such-index"); err == nil {
		t.Fatal("expected error for missing index")
	}
}

func TestSelect(t *testing.T) {
	all := []LRF{{Code: "TERH"}, {Code: "PROV"}, {Code: "BOS"}}

	if got, unknown := Select(all, nil); len(got) != 3 || unknown != nil {
		t.Errorf("empty selection: got %d, unknown %v; want all 3", len(got), unknown)
	}

	got, unknown := Select(all, []string{"terh", " BOS ", "NOPE", ""})
	if len(got) != 2 || got[0].Code != "TERH" || got[1].Code != "BOS" {
		t.Errorf("selected %+v, want TERH and BOS", got)
	}
	if len(unknown) != 1 || unknown[0] != "NOPE" {
		t.Errorf("unknown = %v, want [NOPE]", unknown)
	}

	if got, unknown := Select(all, []string{"XXXX"}); len(got) != 0 || len(unknown) != 1 {
		t.Errorf("no valid codes: got %d selected, unknown %v", len(got), unknown)
	}
}

func TestIndexBodyLookupMode(t *testing.T) {
	var body struct {
		Settings map[string]string `json:"settings"`
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(IndexBody), &body); err != nil {
		t.Fatal(err)
	}
	if body.Settings["index.mode"] != "lookup" {
		t.Errorf("index.mode = %q, want lookup", body.Settings["index.mode"])
	}
	for _, f := range []string{"code", "city", "state", "location"} {
		if _, ok := body.Mappings.Properties[f]; !ok {
			t.Errorf("mapping missing %s", f)
		}
	}
}
