// Command demodreplay publishes demod telemetry from an Elasticsearch export
// to a RabbitMQ exchange, to test tropobeat's demod stream without inSITE.
//
// It reads the sample tarball (sample/log-metric-p-rfdemod-*.tar.gz, whose
// *-data.json file holds one exported document per line) or such a
// *-data.json file directly, and publishes each document's _source as one
// message, in timestamp order. With -now the timestamps are shifted so the
// first report is stamped with the current time, and -speed sets the pace
// (1 = real time, 10 = ten times faster, 0 = as fast as possible).
//
//	go run ./tools/demodreplay -declare -now -speed 10 \
//	    sample/log-metric-p-rfdemod-2026.10.13.tar.gz
//
// For the correlation engine, -scenario injects conditions (BER spikes, MER
// drops, fades, lock loss, dropped reports, ...) on chosen LRFs, channels
// and sides at set times, and -quiet makes those LRFs' own readings healthy
// so only the injections show. -loop replays the sample endlessly, each pass
// continuing the timestamps of the last, for unattended demos:
//
//	go run ./tools/demodreplay -declare -now -loop -quiet \
//	    -scenario tools/demodreplay/scenarios/demo.yml \
//	    sample/log-metric-p-rfdemod-2026.10.13.tar.gz
//
// A lost connection is retried with backoff; reports due while
// disconnected are skipped rather than sent in a burst afterwards.
//
// The exchange and routing key default to tropobeat's (x.tropobeat.demod,
// demod.metrics).
//
// The password is read from RABBITMQ_PASSWORD when -password is not given.
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	var (
		host       = flag.String("host", "localhost", "RabbitMQ host")
		port       = flag.Int("port", 8910, "RabbitMQ port")
		vhost      = flag.String("vhost", "/", "RabbitMQ virtual host")
		user       = flag.String("username", "insite", "RabbitMQ username")
		password   = flag.String("password", "", "RabbitMQ password (default: env RABBITMQ_PASSWORD)")
		exchange   = flag.String("exchange", "x.tropobeat.demod", "exchange to publish to")
		kind       = flag.String("exchange-type", "direct", "exchange type, used with -declare")
		declare    = flag.Bool("declare", false, "declare the exchange (durable) if it doesn't exist")
		routingKey = flag.String("routing-key", "demod.metrics", "routing key for every message")
		now        = flag.Bool("now", false, "shift timestamps so the first report is stamped now")
		speed      = flag.Float64("speed", 1, "replay speed: 1 = real time, 10 = 10x faster, 0 = as fast as possible")
		limit      = flag.Int("limit", 0, "publish at most this many reports (0 = all)")
		loop       = flag.Bool("loop", false, "replay the sample endlessly, continuing the timestamps (needs -now)")
		scenario   = flag.String("scenario", "", "scenario file of timed injections (see scenarios/)")
		quiet      = flag.Bool("quiet", false, "with -scenario: healthy readings on the scenario's LRFs, so only injections show")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: demodreplay [flags] <sample .tar.gz | -data.json file>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *exchange == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *loop && !*now {
		log.Fatal("-loop needs -now, so each pass continues from the current time")
	}
	if *password == "" {
		*password = os.Getenv("RABBITMQ_PASSWORD")
	}

	docs, err := load(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Loaded %d reports from %s (%s to %s)", len(docs), flag.Arg(0),
		docs[0].at.Format(time.RFC3339), docs[len(docs)-1].at.Format(time.RFC3339))

	var sc *Scenario
	if *scenario != "" {
		if sc, err = loadScenario(*scenario); err != nil {
			log.Fatal(err)
		}
		repeat := "runs once"
		if sc.repeat > 0 {
			repeat = "repeats every " + sc.repeat.String()
		}
		log.Printf("Scenario %s: %d steps, %s", *scenario, len(sc.Steps), repeat)
	} else if *quiet {
		log.Fatal("-quiet needs -scenario")
	}
	rw := newRewriter(sc, *quiet, sampleStats(docs))

	uri := amqp.URI{Scheme: "amqp", Host: *host, Port: *port, Username: *user, Password: *password, Vhost: *vhost}
	pub := &amqpPublisher{uri: uri.String(), exchange: *exchange, kind: *kind, key: *routingKey, declare: *declare}
	if err := pub.connect(); err != nil {
		log.Fatalf("connecting to %s:%d: %v", *host, *port, err)
	}
	defer pub.close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &replayer{pub: pub, rw: rw, sleep: sleepCtx, now: time.Now,
		opts: replayOptions{shiftNow: *now, speed: *speed, limit: *limit, loop: *loop}}
	if err := r.run(ctx, docs); err != nil {
		log.Fatal(err)
	}
}

// replayOptions are the replay flags.
type replayOptions struct {
	shiftNow bool
	speed    float64
	limit    int
	loop     bool
}

// replayer publishes the sample, pass after pass with -loop.
type replayer struct {
	pub   publisher
	rw    *rewriter
	opts  replayOptions
	sleep func(context.Context, time.Duration) bool
	now   func() time.Time

	published, skipped, withheld int
	activeSteps                  map[string]bool
	lastStatus                   time.Time
}

// passPeriod is how far each pass's timestamps move on from the previous
// pass: the sample's span rounded up to a whole minute. Each input reports
// about once a minute, so its first report of a pass follows its last
// report of the previous pass by about a minute (at most two), never by
// enough to look like an outage.
func passPeriod(docs []*doc) time.Duration {
	span := docs[len(docs)-1].at.Sub(docs[0].at)
	return span.Truncate(time.Minute) + time.Minute
}

// run publishes the reports in order, paced by speed, until they are all
// sent (or, with loop, until ctx ends).
func (r *replayer) run(ctx context.Context, docs []*doc) error {
	first := docs[0].at
	period := passPeriod(docs)
	start := r.now()
	shift := time.Duration(0)
	if r.opts.shiftNow {
		shift = start.Sub(first)
	}
	r.activeSteps = map[string]bool{}
	r.lastStatus = start
	// Reports due before resumeAt were due while disconnected; they are
	// skipped rather than sent in a burst.
	var resumeAt time.Time

	for pass := 0; ; pass++ {
		for _, d := range docs {
			off := d.at.Sub(first) + time.Duration(pass)*period
			if r.opts.speed > 0 {
				due := start.Add(time.Duration(float64(off) / r.opts.speed))
				if !r.sleep(ctx, due.Sub(r.now())) {
					r.logStatus(pass, off, "Stopped")
					return nil
				}
				if due.Before(resumeAt) {
					r.skipped++
					continue
				}
			} else if ctx.Err() != nil {
				r.logStatus(pass, off, "Stopped")
				return nil
			}
			r.logSteps(off)
			if r.now().Sub(r.lastStatus) >= time.Minute {
				r.logStatus(pass, off, "Status")
			}

			c := d.clone()
			if !r.rw.apply(c, off) {
				r.withheld++
				continue
			}
			ts := d.at.Add(shift + time.Duration(pass)*period)
			c.source["@timestamp"] = ts.Format(time.RFC3339Nano)
			body, err := json.Marshal(c.source)
			if err != nil {
				return err
			}
			if err := r.pub.publish(ctx, body, ts); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				log.Printf("Publishing failed (%v); reconnecting", err)
				r.skipped++
				if !reconnect(ctx, r.pub, r.sleep) {
					return nil
				}
				resumeAt = r.now()
				continue
			}
			r.published++
			if r.opts.limit > 0 && r.published >= r.opts.limit {
				r.logStatus(pass, off, "Limit reached")
				return nil
			}
		}
		if !r.opts.loop {
			r.logStatus(pass, time.Duration(pass+1)*period, "Finished")
			return nil
		}
		log.Printf("Pass %d done; next pass continues %s later", pass+1, period)
	}
}

// logSteps logs scenario steps starting and ending.
func (r *replayer) logSteps(off time.Duration) {
	sc := r.rw.sc
	if sc == nil {
		return
	}
	idx, rep := sc.active(off)
	now := map[string]bool{}
	for _, i := range idx {
		st := sc.Steps[i]
		key := fmt.Sprintf("%d/%d", i, rep)
		now[key] = true
		if !r.activeSteps[key] {
			log.Printf("Scenario step started: %s (%s on %s RF %s side %s, for %s; repetition %d)",
				st.Name, st.Action, st.LRF, rfLabel(st), st.Side, time.Duration(st.Duration), rep+1)
		}
	}
	for key := range r.activeSteps {
		if !now[key] {
			var i, rep int
			fmt.Sscanf(key, "%d/%d", &i, &rep)
			log.Printf("Scenario step ended: %s", sc.Steps[i].Name)
		}
	}
	r.activeSteps = now
}

func rfLabel(st Step) string {
	if st.rf == 0 {
		return "*"
	}
	return fmt.Sprint(st.rf)
}

// logStatus logs progress: the pass, reports published, skipped and
// withheld, the active steps and the time to the next one.
func (r *replayer) logStatus(pass int, off time.Duration, what string) {
	r.lastStatus = r.now()
	msg := fmt.Sprintf("%s: pass %d, %d published, %d skipped while disconnected, %d withheld by drop steps",
		what, pass+1, r.published, r.skipped, r.withheld)
	if sc := r.rw.sc; sc != nil {
		idx, _ := sc.active(off)
		var names []string
		for _, i := range idx {
			names = append(names, sc.Steps[i].Name)
		}
		if len(names) == 0 {
			names = []string{"none"}
		}
		msg += "; active steps: " + strings.Join(names, ", ")
		if d, ok := sc.next(off); ok {
			if r.opts.speed > 0 {
				d = time.Duration(float64(d) / r.opts.speed)
			}
			msg += fmt.Sprintf("; next step in %s", d.Round(time.Second))
		}
	}
	log.Print(msg)
}

// load reads the exported documents from a sample tarball or a -data.json
// file and returns them sorted by timestamp.
func load(path string) ([]*doc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%s: no *-data.json file in the archive", path)
			}
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(h.Name, "-data.json") {
				r = tr
				break
			}
		}
	}

	var out []*doc
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for line := 1; sc.Scan(); line++ {
		var doc struct {
			Source map[string]json.RawMessage `json:"_source"`
		}
		if err := json.Unmarshal(sc.Bytes(), &doc); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		var ts string
		if err := json.Unmarshal(doc.Source["@timestamp"], &ts); err != nil {
			return nil, fmt.Errorf("%s line %d: no @timestamp", path, line)
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		d, err := newDoc(at, doc.Source)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no reports", path)
	}
	slices.SortStableFunc(out, func(a, b *doc) int { return a.at.Compare(b.at) })
	return out, nil
}
