package beater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	tropoCfg "github.com/thetherington/tropobeat/config"
	"github.com/thetherington/tropobeat/internal/demod"
	"github.com/thetherington/tropobeat/internal/elastic"
	"github.com/thetherington/tropobeat/internal/openmeteo"
	"github.com/thetherington/tropobeat/internal/rabbitmqclient"
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
	// baselineJob and weatherJob are the scheduled baseline run and weather
	// poll. Their first runs are triggered by the end of the first discovery
	// run (see afterDiscovery), so both start with the co-channel paths.
	baselineJob    gocron.Job
	weatherJob     gocron.Job
	afterDiscovery sync.Once
	// mock serves simulated weather when openmeteo.mock is enabled.
	mock *http.Server

	// rabbit consumes the live demod report stream. Nil when
	// rabbitmq.enabled is false.
	rabbit *rabbitmqclient.Client
	// baselines holds the latest 7-day demod baselines (baseline job).
	baselines *baselineStore
	// ducting holds the latest ducting result of every co-channel path
	// (weather poll).
	ducting *ductingStore
}

// New creates an instance of tropobeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := tropoCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}
	logger := logp.NewLogger("tropobeat")

	// The weather client. It stays nil when weather polling is off, and the
	// weather job is then never scheduled.
	var (
		weather    openmeteo.Client
		mockServer *http.Server
	)
	if c.OpenMeteo.Enabled {
		omCfg := c.OpenMeteo.Config
		if c.OpenMeteo.Mock.Enabled {
			// Mock weather (openmeteo.mock): instead of calling Open-Meteo,
			// serve simulated weather from a local HTTP server on 127.0.0.1
			// (random free port) that speaks the Open-Meteo forecast format.
			// The client is pointed at that server, so everything after the
			// fetch (batching, de-duplication, the ducting calculation and
			// the events) runs exactly as it would with the real API. Only
			// the weather is simulated: LRFs, demod channels, FCC
			// transmitters and co-channel discovery stay real.
			var err error
			mockServer, omCfg.URL, err = startMockWeather(c.OpenMeteo.Mock)
			if err != nil {
				return nil, err
			}
			// The mock needs no API key. Clearing it also keeps the key from
			// being sent to the local server.
			omCfg.APIKey = ""
			// Make the mode obvious in the logs. runWeather tags every
			// weather and ducting event "mock-weather" (see tagMock), so
			// simulated data can be filtered out or deleted later.
			logger.Warnf("Using MOCK weather (openmeteo.mock, scenario %s, cycle %s); weather and ducting events are simulated and tagged %q",
				c.OpenMeteo.Mock.Scenario, c.OpenMeteo.Mock.Cycle, mockWeatherTag)
		}
		var err error
		weather, err = openmeteo.New(omCfg)
		if err != nil {
			// stopMockWeather does nothing when the mock isn't running; this
			// and the later error returns shut it down so a failed start
			// doesn't leave the server listening.
			stopMockWeather(mockServer)
			return nil, fmt.Errorf("creating Open-Meteo client: %w", err)
		}
		if p := c.OpenMeteo.CoordinatePrecision; p > coordinatePrecisionWarn {
			logger.Warnf("openmeteo.coordinate_precision is %v°; points in different weather-model grid cells may be merged", p)
		}
	} else {
		logger.Info("Weather polling disabled (openmeteo.enabled: false); running co-channel discovery only")
	}

	// The demod stream client. Only the configuration is checked here; it
	// connects when Run starts consuming, so start-up never waits for the
	// broker.
	var rabbit *rabbitmqclient.Client
	if c.RabbitMQ.Enabled {
		var err error
		rabbit, err = rabbitmqclient.New(c.RabbitMQ, logger.Named("rabbitmq"))
		if err != nil {
			stopMockWeather(mockServer)
			return nil, fmt.Errorf("configuring RabbitMQ: %w", err)
		}
	} else {
		logger.Info("Demod stream disabled (rabbitmq.enabled: false)")
	}

	es, err := elastic.NewClient(c.Elasticsearch)
	if err != nil {
		stopMockWeather(mockServer)
		return nil, fmt.Errorf("creating Elasticsearch client: %w", err)
	}

	scheduler, err := newScheduler(logger)
	if err != nil {
		stopMockWeather(mockServer)
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
		mock:      mockServer,
		rabbit:    rabbit,
		baselines: newBaselineStore(c.Baseline.MaxAge),
		ducting:   newDuctingStore(c.Period),
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

	return bt.consume()
}

// consume runs until tropobeat stops, reading the live demod stream when
// RabbitMQ is enabled. Each message is decoded into a demod report and
// acknowledged; malformed ones are rejected without requeueing. In Phase 4a
// the reports are only counted, for the ingest summary logged every minute.
func (bt *tropobeat) consume() error {
	// deliveries stays nil when RabbitMQ is off. A nil channel never
	// receives, so the loop then just waits for shutdown.
	var deliveries <-chan amqp.Delivery
	if bt.rabbit != nil {
		deliveries = bt.rabbit.Consume(bt.ctx)
	}
	summary := time.NewTicker(ingestSummaryInterval)
	defer summary.Stop()

	stats := newIngestStats()
	for {
		select {
		case <-bt.ctx.Done():
			return nil

		case d, ok := <-deliveries:
			if !ok {
				// The client closes the channel only on shutdown. Stop
				// selecting on it rather than spinning on a closed channel.
				deliveries = nil
				continue
			}
			var r demod.Report
			if err := json.Unmarshal(d.Body, &r); err != nil || !r.Valid() {
				_ = d.Nack(false, false)
				stats.reject(err)
				continue
			}
			_ = d.Ack(false)
			stats.count(r)

		case <-summary.C:
			if bt.rabbit != nil {
				bt.logger.Info(stats.summary(bt.rabbit.Reconnects()))
				stats = newIngestStats()
			}
		}
	}
}

func (bt *tropobeat) registerJobs() error {
	// Co-channel discovery runs on its cron schedule and once at start-up.
	_, err := bt.scheduler.NewJob(
		gocron.CronJob(bt.config.Cochannel.Schedule, false),
		gocron.NewTask(bt.runCochannel),
		bt.jobOptions("cochannel", gocron.WithStartAt(gocron.WithStartImmediately()))...,
	)
	if err != nil {
		return fmt.Errorf("scheduling cochannel job: %w", err)
	}

	// Baselines are computed on their cron schedule, and once at start-up
	// when the first discovery run ends (startAfterDiscoveryOnce), so the
	// published baselines list their co-channel paths from the outset.
	bt.baselineJob, err = bt.scheduler.NewJob(
		gocron.CronJob(bt.config.Baseline.Schedule, false),
		gocron.NewTask(bt.runBaseline),
		bt.jobOptions("baseline")...,
	)
	if err != nil {
		return fmt.Errorf("scheduling baseline job: %w", err)
	}

	if bt.weather == nil {
		return nil // weather polling disabled
	}

	// The weather poll runs every period. It isn't started immediately:
	// startAfterDiscoveryOnce runs it when the first discovery run ends, so
	// its first poll covers the co-channel paths rather than only the LRFs.
	bt.weatherJob, err = bt.scheduler.NewJob(
		gocron.DurationJob(bt.config.Period),
		gocron.NewTask(bt.runWeather),
		bt.jobOptions("weather")...,
	)
	if err != nil {
		return fmt.Errorf("scheduling weather job: %w", err)
	}
	return nil
}

// jobOptions returns the options shared by every job, followed by extra.
// A job never overlaps itself: a run that is due while the previous one is
// still going is skipped. Errors returned by a run are logged.
func (bt *tropobeat) jobOptions(name string, extra ...gocron.JobOption) []gocron.JobOption {
	opts := []gocron.JobOption{
		gocron.WithName(name),
		gocron.WithContext(bt.ctx),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
		gocron.WithEventListeners(gocron.AfterJobRunsWithError(func(_ uuid.UUID, name string, err error) {
			bt.logger.Errorf("Job %s failed: %v", name, err)
		})),
	}
	return append(opts, extra...)
}

// startAfterDiscoveryOnce runs the first baseline run and weather poll. It is
// called when the first discovery run ends, whether it succeeded (paths
// available) or failed (baselines are published without paths, and the poll
// falls back to the LRFs only).
func (bt *tropobeat) startAfterDiscoveryOnce() {
	bt.afterDiscovery.Do(func() {
		for name, j := range map[string]gocron.Job{"baseline run": bt.baselineJob, "weather poll": bt.weatherJob} {
			if j == nil {
				continue
			}
			if err := j.RunNow(); err != nil {
				bt.logger.Errorf("Starting first %s: %v", name, err)
			}
		}
	})
}

// Stop stops tropobeat.
func (bt *tropobeat) Stop() {
	bt.cancel()
	if bt.rabbit != nil {
		bt.rabbit.Close()
	}
	if err := bt.scheduler.Shutdown(); err != nil {
		bt.logger.Warnf("Stopping scheduler: %v", err)
	}
	stopMockWeather(bt.mock)
	if bt.client != nil {
		bt.client.Close()
	}
}
