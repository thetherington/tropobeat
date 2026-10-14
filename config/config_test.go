//go:build !integration
// +build !integration

package config

import (
	"slices"
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/config"
)

func TestUnpackOpenMeteo(t *testing.T) {
	raw, err := config.NewConfigFrom(`
period: 1h
openmeteo:
  api_key: secret
  batch_size: 25
  pressure_levels: [1000, 850]
`)
	if err != nil {
		t.Fatal(err)
	}

	c := DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}

	om := c.OpenMeteo
	if c.Period != time.Hour || om.APIKey != "secret" || om.BatchSize != 25 {
		t.Errorf("configured values not unpacked: period=%v %+v", c.Period, om)
	}
	if !slices.Equal(om.PressureLevels, []int{1000, 850}) {
		t.Errorf("pressure_levels = %v, want [1000 850]", om.PressureLevels)
	}
	if om.ForecastDays != 1 || om.Timeout != 10*time.Second || om.MaxRetries != 2 {
		t.Errorf("defaults not kept: %+v", om)
	}
}

func TestDefaultOpenMeteoHasNoAPIKey(t *testing.T) {
	c := DefaultConfig
	if err := config.NewConfig().Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if c.OpenMeteo.APIKey != "" || c.OpenMeteo.URL != "" {
		t.Errorf("default should use the free API: %+v", c.OpenMeteo)
	}
}

func TestUnpackElasticsearch(t *testing.T) {
	raw, err := config.NewConfigFrom(`
elasticsearch:
  hosts: ["https://es1:9200", "https://es2:9200"]
  username: tropobeat
  password: changeme
  ca_cert: /etc/tropobeat/ca.pem
lrf_index: my-lrfs
`)
	if err != nil {
		t.Fatal(err)
	}

	c := DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}

	es := c.Elasticsearch
	if !slices.Equal(es.Hosts, []string{"https://es1:9200", "https://es2:9200"}) {
		t.Errorf("hosts = %v", es.Hosts)
	}
	if es.Username != "tropobeat" || es.Password != "changeme" || es.CACert != "/etc/tropobeat/ca.pem" {
		t.Errorf("credentials not unpacked: %+v", es)
	}
	if c.LRFIndex != "my-lrfs" {
		t.Errorf("lrf_index = %q, want my-lrfs", c.LRFIndex)
	}
}

func TestDefaultElasticsearch(t *testing.T) {
	c := DefaultConfig
	if err := config.NewConfig().Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Elasticsearch.Hosts, []string{"http://localhost:9200"}) {
		t.Errorf("hosts = %v, want localhost default", c.Elasticsearch.Hosts)
	}
	if c.LRFIndex != "lrf-coordinates" {
		t.Errorf("lrf_index = %q, want lrf-coordinates", c.LRFIndex)
	}
}

func TestUnpackDoesNotModifyDefaults(t *testing.T) {
	raw, err := config.NewConfigFrom(`elasticsearch.hosts: ["https://other:9200"]`)
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if got := DefaultConfig.Elasticsearch.Hosts; !slices.Equal(got, []string{"http://localhost:9200"}) {
		t.Fatalf("unpacking modified DefaultConfig hosts: %v", got)
	}
}

func TestOpenMeteoEnabled(t *testing.T) {
	c := DefaultConfig
	if err := config.NewConfig().Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if !c.OpenMeteo.Enabled {
		t.Error("weather polling should be enabled by default")
	}

	raw, err := config.NewConfigFrom(`
openmeteo:
  enabled: false
  api_key: secret
  coordinate_precision: 0.01
`)
	if err != nil {
		t.Fatal(err)
	}
	c = DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if c.OpenMeteo.Enabled {
		t.Error("enabled: false not applied")
	}
	// The client settings sit inline beside enabled.
	if c.OpenMeteo.APIKey != "secret" || c.OpenMeteo.CoordinatePrecision != 0.01 || c.OpenMeteo.BatchSize != 50 {
		t.Errorf("inline client settings not unpacked: %+v", c.OpenMeteo.Config)
	}
}

func TestUnpackDucting(t *testing.T) {
	raw, err := config.NewConfigFrom(`
ducting:
  publish: notable
  score:
    ducting_score: 0.5
`)
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}
	d := c.Ducting
	if d.Publish != "notable" || d.Score.DuctingScore != 0.5 {
		t.Errorf("configured values not unpacked: %+v", d)
	}
	if d.MinLayerM != 20 || d.Score.DuctingNPerKm != -157 {
		t.Errorf("defaults not kept: %+v", d)
	}
}

func TestUnpackDuctingRejectsUnknownPublish(t *testing.T) {
	raw, err := config.NewConfigFrom(`ducting.publish: some`)
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig
	if err := raw.Unpack(&c); err == nil {
		t.Error("ducting.publish: some should fail validation")
	}
}
