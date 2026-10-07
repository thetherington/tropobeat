// Config is put into a different package to prevent cyclic imports in case
// it is needed in several locations

package config

import (
	"time"

	"github.com/thetherington/tropobeat/beater/openmeteo"
)

type Config struct {
	Period    time.Duration    `config:"period"`
	OpenMeteo openmeteo.Config `config:"openmeteo"`
}

var DefaultConfig = Config{
	Period:    1 * time.Second,
	OpenMeteo: openmeteo.DefaultConfig,
}
