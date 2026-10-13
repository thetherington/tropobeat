package beater

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"

	tropoCfg "github.com/thetherington/tropobeat/config"
	"github.com/thetherington/tropobeat/internal/elastic"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// coordinatePrecisionWarn is the coordinate_precision above which points in
// different weather-model grid cells may be merged.
const coordinatePrecisionWarn = 0.1

// tropobeat configuration.
type tropobeat struct {
	ctx    context.Context
	cancel context.CancelFunc
	config tropoCfg.Config
	client beat.Client
	logger *logp.Logger

	// es reads reference data (LRF sites, FCC transmitters, demod telemetry).
	es *elasticsearch.Client
	// weather queries the Open-Meteo API. Nil when weather polling is
	// disabled.
	weather openmeteo.Client
	// scheduler runs the co-channel discovery and weather poll jobs.
	scheduler gocron.Scheduler
	// state holds the latest discovery snapshot.
	state atomic.Pointer[snapshot]
	// weatherJob is the scheduled weather poll. Its first run is triggered
	// by the end of the first discovery run (see firstPoll).
	weatherJob gocron.Job
	firstPoll  sync.Once
}

// New creates an instance of tropobeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := tropoCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}
	logger := logp.NewLogger("tropobeat")

	var weather openmeteo.Client
	if c.OpenMeteo.Enabled {
		var err error
		weather, err = openmeteo.New(c.OpenMeteo.Config)
		if err != nil {
			return nil, fmt.Errorf("creating Open-Meteo client: %w", err)
		}
		if p := c.OpenMeteo.CoordinatePrecision; p > coordinatePrecisionWarn {
			logger.Warnf("openmeteo.coordinate_precision is %v°; points in different weather-model grid cells may be merged", p)
		}
	} else {
		logger.Info("Weather polling disabled (openmeteo.enabled: false); running co-channel discovery only")
	}

	es, err := elastic.NewClient(c.Elasticsearch)
	if err != nil {
		return nil, fmt.Errorf("creating Elasticsearch client: %w", err)
	}

	scheduler, err := newScheduler(logger)
	if err != nil {
		return nil, fmt.Errorf("creating scheduler: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	bt := &tropobeat{
		ctx:       ctx,
		cancel:    cancel,
		config:    c,
		logger:    logger,
		es:        es,
		weather:   weather,
		scheduler: scheduler,
	}
	return bt, nil
}

// Run starts tropobeat.
func (bt *tropobeat) Run(b *beat.Beat) error {
	bt.logger.Info("tropobeat is running! Hit CTRL-C to stop it.")

	var err error
	bt.client, err = b.Publisher.Connect()
	if err != nil {
		return err
	}

	// Check the LRF list before starting, and seed the snapshot so the
	// weather poll has LRFs before the first discovery run finishes.
	_, selected, err := bt.loadLRFs(bt.ctx)
	if err != nil {
		return err
	}
	bt.state.Store(&snapshot{LRFs: selected})
	bt.logger.Infof("Loaded %d LRF sites from index %s", len(selected), bt.config.LRFIndex)

	if err := bt.registerJobs(); err != nil {
		return err
	}
	bt.scheduler.Start()

	<-bt.ctx.Done()
	return nil
}

func (bt *tropobeat) registerJobs() error {
	jobs := []struct {
		name string
		def  gocron.JobDefinition
		run  func(context.Context) error
		// startNow runs the job at start-up. The weather poll instead starts
		// when the first discovery run ends, so its first poll covers the
		// co-channel paths rather than only the LRFs.
		startNow bool
		job      *gocron.Job
	}{
		{name: "cochannel", def: gocron.CronJob(bt.config.Cochannel.Schedule, false), run: bt.runCochannel, startNow: true},
		{name: "weather", def: gocron.DurationJob(bt.config.Period), run: bt.runWeather, job: &bt.weatherJob},
	}
	if bt.weather == nil {
		jobs = jobs[:1] // weather polling disabled
	}
	for _, j := range jobs {
		opts := []gocron.JobOption{
			gocron.WithName(j.name),
			gocron.WithContext(bt.ctx),
			gocron.WithSingletonMode(gocron.LimitModeReschedule),
			gocron.WithEventListeners(gocron.AfterJobRunsWithError(func(_ uuid.UUID, name string, err error) {
				bt.logger.Errorf("Job %s failed: %v", name, err)
			})),
		}
		if j.startNow {
			opts = append(opts, gocron.WithStartAt(gocron.WithStartImmediately()))
		}
		job, err := bt.scheduler.NewJob(j.def, gocron.NewTask(j.run), opts...)
		if err != nil {
			return fmt.Errorf("scheduling %s job: %w", j.name, err)
		}
		if j.job != nil {
			*j.job = job
		}
	}
	return nil
}

// startWeatherOnce runs the first weather poll. It is called when the first
// discovery run ends, whether it succeeded (paths available) or failed (the
// poll falls back to the LRFs only).
func (bt *tropobeat) startWeatherOnce() {
	bt.firstPoll.Do(func() {
		if bt.weatherJob == nil {
			return
		}
		if err := bt.weatherJob.RunNow(); err != nil {
			bt.logger.Errorf("Starting first weather poll: %v", err)
		}
	})
}

// Stop stops tropobeat.
func (bt *tropobeat) Stop() {
	bt.cancel()
	if err := bt.scheduler.Shutdown(); err != nil {
		bt.logger.Warnf("Stopping scheduler: %v", err)
	}
	if bt.client != nil {
		bt.client.Close()
	}
}
