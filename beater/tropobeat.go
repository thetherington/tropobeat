package beater

import (
	"context"
	"fmt"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/elastic/go-elasticsearch/v8"

	"github.com/thetherington/tropobeat/beater/openmeteo"
	tropoCfg "github.com/thetherington/tropobeat/config"
	"github.com/thetherington/tropobeat/elastic"
	"github.com/thetherington/tropobeat/lrf"
)

// tropobeat configuration.
type tropobeat struct {
	ctx    context.Context
	cancel context.CancelFunc
	config tropoCfg.Config
	client beat.Client
	logger *logp.Logger

	// es reads reference data (LRF sites) from Elasticsearch.
	es *elasticsearch.Client
	// weather queries the Open-Meteo API.
	weather openmeteo.Client
	// lrfs are the LRF sites loaded from config.LRFIndex.
	lrfs []lrf.LRF
}

// New creates an instance of tropobeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := tropoCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}

	weather, err := openmeteo.New(c.OpenMeteo)
	if err != nil {
		return nil, fmt.Errorf("creating Open-Meteo client: %w", err)
	}

	es, err := elastic.NewClient(c.Elasticsearch)
	if err != nil {
		return nil, fmt.Errorf("creating Elasticsearch client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	bt := &tropobeat{
		ctx:     ctx,
		cancel:  cancel,
		config:  c,
		logger:  logp.NewLogger("tropobeat"),
		es:      es,
		weather: weather,
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

	bt.lrfs, err = lrf.Load(bt.ctx, bt.es, bt.config.LRFIndex)
	if err != nil {
		return fmt.Errorf("loading LRF sites from index %s: %w", bt.config.LRFIndex, err)
	}
	if len(bt.lrfs) == 0 {
		return fmt.Errorf("no LRF sites found in index %s; load them with `tropobeat insert lrfcoordinates`", bt.config.LRFIndex)
	}
	bt.logger.Infof("Loaded %d LRF sites from index %s", len(bt.lrfs), bt.config.LRFIndex)

	ticker := time.NewTicker(bt.config.Period)
	defer ticker.Stop()
	counter := 1
	for {
		select {
		case <-bt.ctx.Done():
			return nil
		case <-ticker.C:
		}

		event := beat.Event{
			Timestamp: time.Now(),
			Fields: mapstr.M{
				"type":    b.Info.Name,
				"counter": counter,
			},
		}
		bt.client.Publish(event)
		bt.logger.Info("Event sent")
		counter++
	}
}

// Stop stops tropobeat.
func (bt *tropobeat) Stop() {
	bt.cancel()
	if bt.client != nil {
		bt.client.Close()
	}
}
