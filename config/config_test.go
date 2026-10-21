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

func TestOpenMeteoMock(t *testing.T) {
	c := DefaultConfig
	if err := config.NewConfig().Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if c.OpenMeteo.Mock.Enabled || c.OpenMeteo.Mock.Scenario != "mixed" || c.OpenMeteo.Mock.Cycle != 6*time.Hour {
		t.Errorf("mock defaults: %+v", c.OpenMeteo.Mock)
	}

	raw, err := config.NewConfigFrom(`
openmeteo:
  mock:
    enabled: true
    scenario: trapped
    cycle: 30m
`)
	if err != nil {
		t.Fatal(err)
	}
	c = DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if !c.OpenMeteo.Mock.Enabled || c.OpenMeteo.Mock.Scenario != "trapped" || c.OpenMeteo.Mock.Cycle != 30*time.Minute {
		t.Errorf("mock settings not unpacked: %+v", c.OpenMeteo.Mock)
	}

	bad, _ := config.NewConfigFrom(`openmeteo.mock.scenario: stormy`)
	c = DefaultConfig
	if err := bad.Unpack(&c); err == nil {
		t.Error("an unknown mock scenario should be rejected")
	}
}

func TestUnpackRabbitMQAndBaseline(t *testing.T) {
	raw, err := config.NewConfigFrom(`
rabbitmq:
  enabled: true
  host: rabbit.example
  password: secret
  exchange: x.rfdemod
  routing_key: rfdemod.data
  ssl:
    enabled: true
    ca_cert: /etc/tropobeat/rabbit-ca.pem
baseline:
  window_days: 3
`)
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig
	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}
	r := c.RabbitMQ
	if !r.Enabled || r.Host != "rabbit.example" || r.Password != "secret" || r.Exchange != "x.rfdemod" || r.RoutingKey != "rfdemod.data" {
		t.Errorf("rabbitmq not unpacked: %+v", r)
	}
	if !r.SSL.Enabled || r.SSL.CACert != "/etc/tropobeat/rabbit-ca.pem" {
		t.Errorf("rabbitmq.ssl not unpacked: %+v", r.SSL)
	}
	// Defaults kept for keys not set.
	if r.Port != 8910 || r.Queue != "q.tropobeat.demod" || r.ExchangeDeclare != "passive" || r.Prefetch != 500 || r.MessageTTL != 5*time.Minute {
		t.Errorf("rabbitmq defaults not kept: %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	b := c.Baseline
	if b.WindowDays != 3 || b.Schedule != "30 3 * * *" || b.MinSamples != 1440 || b.BERFloor != 1e-6 || b.MaxAge != 48*time.Hour || !b.Publish {
		t.Errorf("baseline = %+v", b)
	}
}

func TestDefaultRabbitMQDisabled(t *testing.T) {
	c := DefaultConfig
	if err := config.NewConfig().Unpack(&c); err != nil {
		t.Fatal(err)
	}
	if c.RabbitMQ.Enabled || c.RabbitMQ.Password != "" {
		t.Errorf("RabbitMQ should be off with no password by default: %+v", c.RabbitMQ)
	}
	if c.RabbitMQ.Exchange != "x.tropobeat.demod" || c.RabbitMQ.RoutingKey != "demod.metrics" {
		t.Errorf("default exchange %q, routing key %q", c.RabbitMQ.Exchange, c.RabbitMQ.RoutingKey)
	}
}

func TestBaselineValidation(t *testing.T) {
	raw, err := config.NewConfigFrom(`
baseline:
  window_days: 0
`)
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig
	if err := raw.Unpack(&c); err == nil {
		t.Error("window_days: 0 accepted")
	}
}
