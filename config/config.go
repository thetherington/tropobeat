// Config is put into a different package to prevent cyclic imports in case
// it is needed in several locations

package config

import (
	"time"

	"github.com/thetherington/tropobeat/beater/openmeteo"
	"github.com/thetherington/tropobeat/elastic"
	"github.com/thetherington/tropobeat/lrf"
)

type Config struct {
	Period time.Duration `config:"period"`
	// Elasticsearch is the cluster tropobeat reads reference data from, such
	// as the LRF sites. Events are still published via the beat output.
	Elasticsearch elastic.Config `config:"elasticsearch"`
	// LRFIndex is the index holding the LRF sites.
	LRFIndex  string           `config:"lrf_index" validate:"required"`
	OpenMeteo openmeteo.Config `config:"openmeteo"`
}

var DefaultConfig = Config{
	Period:        1 * time.Second,
	Elasticsearch: elastic.Config{Hosts: elastic.DefaultHosts},
	LRFIndex:      lrf.DefaultIndex,
	OpenMeteo:     openmeteo.DefaultConfig,
}
