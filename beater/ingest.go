package beater

import (
	"fmt"
	"time"

	"github.com/thetherington/tropobeat/internal/demod"
)

// ingestSummaryInterval is how often the demod stream summary is logged.
const ingestSummaryInterval = time.Minute

// ingestStats counts the demod reports received from RabbitMQ since the
// last summary.
type ingestStats struct {
	decoded, locked, rejected, malformed int
	instances                            map[demod.Instance]struct{}
	lrfs                                 map[string]struct{}
}

func newIngestStats() *ingestStats {
	return &ingestStats{instances: map[demod.Instance]struct{}{}, lrfs: map[string]struct{}{}}
}

// count records a decoded report.
func (s *ingestStats) count(r demod.Report) {
	s.decoded++
	if r.Locked() {
		s.locked++
	}
	s.instances[r.Instance()] = struct{}{}
	s.lrfs[r.LRF] = struct{}{}
}

// reject records a report that was not valid JSON (err set) or was missing
// its instance, LRF, channel or timestamp (err nil).
func (s *ingestStats) reject(err error) {
	s.rejected++
	if err != nil {
		s.malformed++
	}
}

// summary is the log line for the period, with the client's reconnect count.
func (s *ingestStats) summary(reconnects int64) string {
	return fmt.Sprintf("Demod stream: %d reports decoded (%d locked) from %d demod instances at %d LRFs; %d rejected (%d malformed, %d incomplete); %d reconnects since start",
		s.decoded, s.locked, len(s.instances), len(s.lrfs), s.rejected, s.malformed, s.rejected-s.malformed, reconnects)
}
