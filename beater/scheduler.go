package beater

import (
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/go-co-op/gocron/v2"
)

// schedulerStopTimeout bounds how long Stop waits for running jobs.
const schedulerStopTimeout = 30 * time.Second

func newScheduler(logger *logp.Logger) (gocron.Scheduler, error) {
	return gocron.NewScheduler(
		gocron.WithLocation(time.UTC),
		gocron.WithLogger(gocronLogger{logger.Named("scheduler")}),
		gocron.WithStopTimeout(schedulerStopTimeout),
	)
}

// gocronLogger adapts a logp.Logger to gocron's Logger interface. gocron
// passes key/value pairs as args, which map onto logp's structured *w
// methods.
type gocronLogger struct {
	l *logp.Logger
}

func (g gocronLogger) Debug(msg string, args ...any) { g.l.Debugw(msg, args...) }
func (g gocronLogger) Info(msg string, args ...any)  { g.l.Infow(msg, args...) }
func (g gocronLogger) Warn(msg string, args ...any)  { g.l.Warnw(msg, args...) }
func (g gocronLogger) Error(msg string, args ...any) { g.l.Errorw(msg, args...) }
