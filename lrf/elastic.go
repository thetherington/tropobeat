package lrf

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
