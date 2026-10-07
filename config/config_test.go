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
