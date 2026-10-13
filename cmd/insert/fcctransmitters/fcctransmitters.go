// Package fcctransmitters implements the "tropobeat insert fcctransmitters"
// command.
package fcctransmitters

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/thetherington/tropobeat/internal/elastic"
	"github.com/thetherington/tropobeat/internal/fcc"
)

// options holds the flag values for the fcctransmitters command.
type options struct {
	states   []string
	services []string
	statuses []string
	index    string
	recreate bool
	dryRun   bool
	noPrune  bool
	fcc      fcc.Config
	es       elastic.Config
}

// NewCmd returns the "fcctransmitters" subcommand.
func NewCmd() *cobra.Command {
	opts := &options{fcc: fcc.DefaultConfig}

	c := &cobra.Command{
		Use:   "fcctransmitters",
		Short: "Insert FCC TV transmitter locations into Elasticsearch",
		Long: `Fetches TV transmitter records from the FCC TV Query service, one request
per state, and indexes them into Elasticsearch with a geo_point location.

By default only licensed full-power stations (status LIC, service DTV/TV/DT)
are kept. Documents are keyed by facility ID and location, so re-running
updates existing transmitters. After a fully successful run, transmitters in
the fetched states that are no longer listed are removed (disable with
--no-prune).

Intended to run from cron, outside the beat. Elasticsearch credentials can
be supplied via ES_HOSTS, ES_USERNAME, ES_PASSWORD or ES_API_KEY. Example:

  0 3 * * * flock -n /tmp/tropobeat-fcc.lock /opt/tropobeat/tropobeat insert fcctransmitters >>/var/log/tropobeat/fcc.log 2>&1

Exits non-zero if any state fails.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, opts)
		},
	}

	f := c.Flags()
	f.StringSliceVar(&opts.states, "states", fcc.States, "states/territories to fetch")
	f.StringSliceVar(&opts.services, "services", fcc.DefaultFilter.Services, "service codes to keep (e.g. DTV,TV,DT,DTS,DRT,DCA,LPD)")
	f.StringSliceVar(&opts.statuses, "statuses", fcc.DefaultFilter.Statuses, "record statuses to keep (e.g. LIC,CP)")
	f.StringVarP(&opts.index, "index", "i", fcc.DefaultIndex, "Elasticsearch index to insert into")
	f.BoolVar(&opts.recreate, "recreate", false, "delete and recreate the index before inserting")
	f.BoolVar(&opts.dryRun, "dry-run", false, "fetch and print the transmitters without inserting")
	f.BoolVar(&opts.noPrune, "no-prune", false, "keep transmitters no longer listed by the FCC")
	f.DurationVar(&opts.fcc.Delay, "delay", opts.fcc.Delay, "minimum time between FCC requests")
	f.DurationVar(&opts.fcc.Timeout, "timeout", opts.fcc.Timeout, "timeout for each FCC request")
	f.StringVar(&opts.fcc.UserAgent, "user-agent", opts.fcc.UserAgent, "User-Agent sent to the FCC")
	elastic.AddFlags(f, &opts.es)

	return c
}

func run(cmd *cobra.Command, opts *options) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	runStart := time.Now().UTC().Truncate(time.Millisecond)

	states := normalize(opts.states)
	filter := fcc.Filter{Statuses: normalize(opts.statuses), Services: normalize(opts.services)}
	client := fcc.NewClient(opts.fcc)

	var (
		kept                    = map[string]fcc.Transmitter{}
		failed                  []string
		totalRows, totalSkipped int
		errs                    []error
	)
	for _, st := range states {
		ts, skipped, err := client.Fetch(ctx, st)
		if err == nil && len(ts) == 0 {
			// Every state and territory has TV stations, so an empty listing
			// means a bad request or response. Treating it as a failure
			// stops pruning from deleting that state's transmitters.
			err = errors.New("no records listed; unknown state or bad response")
		}
		if err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", st, err)
			failed = append(failed, st)
			errs = append(errs, fmt.Errorf("%s: %w", st, err))
			if errors.Is(err, fcc.ErrRefused) || ctx.Err() != nil {
				fmt.Fprintln(out, "stopping: remaining states not fetched")
				break
			}
			continue
		}

		n := 0
		for _, t := range ts {
			if filter.Match(t) {
				kept[t.DocID()] = t
				n++
			}
		}
		totalRows += len(ts)
		totalSkipped += skipped
		fmt.Fprintf(out, "%s: %d records, %d kept, %d skipped\n", st, len(ts), n, skipped)
	}

	fmt.Fprintf(out, "fetched %d states (%d failed): %d records, %d transmitters kept, %d skipped\n",
		len(states)-len(failed), len(failed), totalRows, len(kept), totalSkipped)

	if opts.dryRun {
		if err := printTable(cmd, kept); err != nil {
			return err
		}
		return errors.Join(errs...)
	}

	if len(kept) == 0 && len(failed) > 0 {
		return fmt.Errorf("no transmitters fetched: %w", errors.Join(errs...))
	}

	opts.es.ApplyEnv()
	es, err := elastic.NewClient(opts.es)
	if err != nil {
		return err
	}

	created, err := elastic.EnsureIndex(ctx, es, opts.index, fcc.IndexBody, opts.recreate)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "created index %s\n", opts.index)
	}

	docs := make([]elastic.Doc, 0, len(kept))
	for id, t := range kept {
		docs = append(docs, elastic.Doc{ID: id, Source: t.Document(runStart)})
	}
	stats, err := elastic.BulkIndex(ctx, es, opts.index, docs)
	fmt.Fprintf(out, "indexed %d transmitters into %s (%d failed)\n", stats.Indexed, opts.index, stats.Failed)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}

	switch {
	case opts.noPrune || created:
	case len(failed) > 0:
		fmt.Fprintf(out, "not pruning: %d state(s) failed\n", len(failed))
	default:
		pruned, err := elastic.DeleteByQuery(ctx, es, opts.index, pruneQuery(states, runStart))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "pruned %d transmitters no longer listed\n", pruned)
	}

	return errors.Join(errs...)
}

// pruneQuery matches transmitters in states that were not refreshed by the
// run that started at runStart. Limiting it to the fetched states means a
// run with --states RI never removes other states' transmitters.
func pruneQuery(states []string, runStart time.Time) string {
	q := map[string]any{
		"bool": map[string]any{
			"filter": []any{
				map[string]any{"terms": map[string]any{"state": states}},
				map[string]any{"range": map[string]any{"fetched_at": map[string]any{"lt": runStart.Format(time.RFC3339Nano)}}},
			},
		},
	}
	b, _ := json.Marshal(q)
	return string(b)
}

func normalize(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func printTable(cmd *cobra.Command, kept map[string]fcc.Transmitter) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CALL SIGN\tSERVICE\tRF\tVIRTUAL\tSTATE\tCITY\tFACILITY\tLATITUDE\tLONGITUDE\tERP kW")
	for _, t := range sortedTransmitters(kept) {
		virtual, erp := "-", "-"
		if t.VirtualChannel != nil {
			virtual = fmt.Sprint(*t.VirtualChannel)
		}
		if t.ERPkW != nil {
			erp = fmt.Sprint(*t.ERPkW)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%.6f\t%.6f\t%s\n",
			t.CallSign, t.Service, t.RFChannel, virtual, t.State, t.City, t.FacilityID, t.Latitude, t.Longitude, erp)
	}
	return w.Flush()
}

func sortedTransmitters(m map[string]fcc.Transmitter) []fcc.Transmitter {
	ts := make([]fcc.Transmitter, 0, len(m))
	for _, t := range m {
		ts = append(ts, t)
	}
	slices.SortFunc(ts, func(a, b fcc.Transmitter) int {
		return cmp.Or(cmp.Compare(a.State, b.State), cmp.Compare(a.CallSign, b.CallSign), cmp.Compare(a.DocID(), b.DocID()))
	})
	return ts
}
