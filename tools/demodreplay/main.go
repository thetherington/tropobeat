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
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// report is one exported document's _source, with its timestamp parsed.
type report struct {
	at     time.Time
	source map[string]json.RawMessage
}

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
	if *password == "" {
		*password = os.Getenv("RABBITMQ_PASSWORD")
	}

	reports, err := load(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	if *limit > 0 && len(reports) > *limit {
		reports = reports[:*limit]
	}
	log.Printf("Loaded %d reports from %s (%s to %s)", len(reports), flag.Arg(0),
		reports[0].at.Format(time.RFC3339), reports[len(reports)-1].at.Format(time.RFC3339))

	uri := amqp.URI{Scheme: "amqp", Host: *host, Port: *port, Username: *user, Password: *password, Vhost: *vhost}
	conn, err := amqp.Dial(uri.String())
	if err != nil {
		log.Fatalf("connecting to %s:%d: %v", *host, *port, err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		log.Fatal(err)
	}
	if *declare {
		if err := ch.ExchangeDeclare(*exchange, *kind, true, false, false, false, nil); err != nil {
			log.Fatalf("declaring exchange %s: %v", *exchange, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := replay(ctx, ch, *exchange, *routingKey, reports, *now, *speed); err != nil {
		log.Fatal(err)
	}
}

// replay publishes the reports in order, paced by speed.
func replay(ctx context.Context, ch *amqp.Channel, exchange, key string, reports []report, now bool, speed float64) error {
	first := reports[0].at
	shift := time.Duration(0)
	if now {
		shift = time.Since(first)
	}
	start := time.Now()
	progress := time.NewTicker(10 * time.Second)
	defer progress.Stop()

	for i, r := range reports {
		// Wait until this report's (scaled) offset from the first.
		if speed > 0 {
			due := start.Add(time.Duration(float64(r.at.Sub(first)) / speed))
			if d := time.Until(due); d > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(d):
				}
			}
		}
		select {
		case <-ctx.Done():
			log.Printf("Stopped after %d reports", i)
			return nil
		case <-progress.C:
			log.Printf("Published %d of %d reports", i, len(reports))
		default:
		}

		ts := r.at.Add(shift)
		r.source["@timestamp"], _ = json.Marshal(ts.Format(time.RFC3339Nano))
		body, err := json.Marshal(r.source)
		if err != nil {
			return err
		}
		err = ch.PublishWithContext(ctx, exchange, key, false, false, amqp.Publishing{
			ContentType: "application/json",
			Timestamp:   ts,
			Body:        body,
		})
		if err != nil {
			return fmt.Errorf("publishing report %d: %w", i, err)
		}
	}
	log.Printf("Published %d reports in %s", len(reports), time.Since(start).Round(time.Millisecond))
	return nil
}

// load reads the exported documents from a sample tarball or a -data.json
// file and returns them sorted by timestamp.
func load(path string) ([]report, error) {
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

	var out []report
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
		out = append(out, report{at: at, source: doc.Source})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no reports", path)
	}
	slices.SortStableFunc(out, func(a, b report) int { return a.at.Compare(b.at) })
	return out, nil
}
