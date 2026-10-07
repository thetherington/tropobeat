package lrf

import (
	"context"

	"github.com/elastic/go-elasticsearch/v8"

	"github.com/thetherington/tropobeat/elastic"
)

// DefaultIndex is the default Elasticsearch index for LRF sites.
const DefaultIndex = "lrf-coordinates"

// IndexBody is the settings and mappings used to create the LRF index.
// Coordinates are stored as a geo_point in the "location" field.
const IndexBody = `{
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "dma_name": { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
      "code":     { "type": "keyword" },
      "street":   { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
      "city":     { "type": "text", "fields": { "keyword": { "type": "keyword" } } },
      "state":    { "type": "keyword" },
      "zip_code": { "type": "keyword" },
      "location": { "type": "geo_point" },
      "gps_dms":  { "type": "keyword", "index": false }
    }
  }
}`

// GeoPoint is an Elasticsearch geo_point in object form.
type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Document is the Elasticsearch representation of an LRF.
type Document struct {
	DMAName  string   `json:"dma_name"`
	Code     string   `json:"code"`
	Street   string   `json:"street"`
	City     string   `json:"city"`
	State    string   `json:"state"`
	ZipCode  string   `json:"zip_code"`
	Location GeoPoint `json:"location"`
	GPSDMS   string   `json:"gps_dms,omitempty"`
}

// Document converts l to its Elasticsearch document form.
func (l LRF) Document() Document {
	return Document{
		DMAName:  l.DMAName,
		Code:     l.Code,
		Street:   l.Street,
		City:     l.City,
		State:    l.State,
		ZipCode:  l.ZipCode,
		Location: GeoPoint{Lat: l.Latitude, Lon: l.Longitude},
		GPSDMS:   l.GPSDMS,
	}
}

// LRF converts an Elasticsearch document back to an LRF.
func (d Document) LRF() LRF {
	return LRF{
		DMAName:   d.DMAName,
		Code:      d.Code,
		Street:    d.Street,
		City:      d.City,
		State:     d.State,
		ZipCode:   d.ZipCode,
		Latitude:  d.Location.Lat,
		Longitude: d.Location.Lon,
		GPSDMS:    d.GPSDMS,
	}
}

// Load returns every LRF stored in index.
func Load(ctx context.Context, client *elasticsearch.Client, index string) ([]LRF, error) {
	docs, err := elastic.SearchAll[Document](ctx, client, index)
	if err != nil {
		return nil, err
	}
	lrfs := make([]LRF, len(docs))
	for i, d := range docs {
		lrfs[i] = d.LRF()
	}
	return lrfs, nil
}
