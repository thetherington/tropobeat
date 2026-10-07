package beater

import (
	"fmt"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"

	countCfg "github.com/thetherington/tropobeat/config"
)

// tropobeat configuration.
type tropobeat struct {
	done   chan struct{}
	config countCfg.Config
	client beat.Client
}

// New creates an instance of tropobeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := countCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}

	bt := &tropobeat{
		done:   make(chan struct{}),
		config: c,
	}
	return bt, nil
}

// Run starts tropobeat.
func (bt *tropobeat) Run(b *beat.Beat) error {
	logp.Info("tropobeat is running! Hit CTRL-C to stop it.")

	var err error
	bt.client, err = b.Publisher.Connect()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(bt.config.Period)
	counter := 1
	for {
		select {
		case <-bt.done:
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
		logp.Info("Event sent")
		counter++
	}
}

// Stop stops tropobeat.
func (bt *tropobeat) Stop() {
	bt.client.Close()
	close(bt.done)
}
