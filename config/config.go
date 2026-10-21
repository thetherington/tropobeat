// Config is put into a different package to prevent cyclic imports in case
// it is needed in several locations

package config

import (
	"time"

	"github.com/thetherington/tropobeat/internal/correlator"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/elastic"
	"github.com/thetherington/tropobeat/internal/fcc"
	"github.com/thetherington/tropobeat/internal/lrf"
	"github.com/thetherington/tropobeat/internal/openmeteo"
	"github.com/thetherington/tropobeat/internal/openmeteo/mock"
	"github.com/thetherington/tropobeat/internal/rabbitmqclient"
)

type Config struct {
	// Period is the weather poll interval.
	Period time.Duration `config:"period" validate:"positive,nonzero"`
	// Elasticsearch is the cluster tropobeat reads reference data from, such
	// as the LRF sites. Events are still published via the beat output.
	Elasticsearch elastic.Config `config:"elasticsearch"`
	// LRFIndex is the index holding the LRF sites.
	LRFIndex string `config:"lrf_index" validate:"required"`
	// LRFSites limits tropobeat to these LRF codes. Empty means all LRFs.
	LRFSites []string `config:"lrf_sites"`
	// DemodIndex is the index pattern holding the demod telemetry.
	DemodIndex string `config:"demod_index" validate:"required"`
	// FCCIndex is the index holding the FCC transmitters.
	FCCIndex  string          `config:"fcc_index" validate:"required"`
	Cochannel CochannelConfig `config:"cochannel"`
	OpenMeteo OpenMeteoConfig `config:"openmeteo"`
	// Ducting configures the ducting calculation run on each weather poll.
	Ducting ducting.Config `config:"ducting"`
	// RabbitMQ is the live demod report stream.
	RabbitMQ rabbitmqclient.Config `config:"rabbitmq"`
	// Baseline configures the daily 7-day demod baselines.
	Baseline BaselineConfig `config:"baseline"`
	// Correlator configures the CCI correlation engine run on the live
	// demod stream.
	Correlator correlator.Config `config:"correlator"`
}

// BaselineConfig configures the daily baseline job: the median Pre-FEC BER
// and MER of every demod instance and channel over the last WindowDays.
type BaselineConfig struct {
	// Schedule is a cron expression (UTC) for the baseline run. The job also
	// runs once at start-up.
	Schedule string `config:"schedule" validate:"required"`
	// WindowDays is how many whole UTC days of demod telemetry, up to the
	// start of today, the medians cover.
	WindowDays int `config:"window_days" validate:"positive,nonzero"`
	// MinSamples is the fewest locked reports a baseline needs to be usable.
	MinSamples int `config:"min_samples" validate:"positive,nonzero"`
	// BERFloor is the smallest effective BER baseline. Most medians are 0,
	// and a spike is measured against max(median, BERFloor).
	BERFloor float64 `config:"ber_floor" validate:"positive,nonzero"`
	// MaxAge is how old the baselines can get (after failed runs) before
	// they are treated as missing.
	MaxAge time.Duration `config:"max_age" validate:"positive,nonzero"`
	// Publish publishes each run's baselines as the baseline metricset.
	Publish bool `config:"publish"`
}

// OpenMeteoConfig is the weather client configuration plus a switch for the
// weather poll.
type OpenMeteoConfig struct {
	// Enabled turns the weather poll on. When false, tropobeat only runs
	// co-channel discovery and makes no Open-Meteo calls.
	Enabled          bool `config:"enabled"`
	openmeteo.Config `config:",inline"`
	// Mock serves simulated weather instead of calling Open-Meteo, for
	// building and testing dashboards.
	Mock mock.Config `config:"mock"`
}

// CochannelConfig configures the daily co-channel discovery job.
type CochannelConfig struct {
	// Schedule is a cron expression (UTC) for the discovery run. The job
	// also runs once at start-up.
	Schedule string `config:"schedule" validate:"required"`
	// Radius is the search radius around each LRF, as an Elasticsearch
	// distance such as "200mi".
	Radius string `config:"radius" validate:"required"`
	// DemodWindow is how far back demod telemetry is read to find each
	// LRF's received channels.
	DemodWindow time.Duration `config:"demod_window" validate:"positive,nonzero"`
}

var DefaultConfig = Config{
	Period:        1 * time.Hour,
	Elasticsearch: elastic.Config{Hosts: elastic.DefaultHosts},
	LRFIndex:      lrf.DefaultIndex,
	DemodIndex:    demod.DefaultIndex,
	FCCIndex:      fcc.DefaultIndex,
	Cochannel: CochannelConfig{
		Schedule:    "0 3 * * *",
		Radius:      "200mi",
		DemodWindow: 24 * time.Hour,
	},
	OpenMeteo: OpenMeteoConfig{Enabled: true, Config: openmeteo.DefaultConfig, Mock: mock.DefaultConfig},
	Ducting:   ducting.DefaultConfig,
	RabbitMQ:  rabbitmqclient.DefaultConfig,
	Baseline: BaselineConfig{
		Schedule:   "30 3 * * *",
		WindowDays: 7,
		MinSamples: 1440,
		BERFloor:   1e-6,
		MaxAge:     48 * time.Hour,
		Publish:    true,
	},
	Correlator: correlator.DefaultConfig,
}
