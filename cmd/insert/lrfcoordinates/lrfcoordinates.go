// Package lrfcoordinates implements the "tropobeat insert lrfcoordinates" command.
package lrfcoordinates

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/thetherington/tropobeat/elastic"
	"github.com/thetherington/tropobeat/lrf"
)

// options holds the flag values for the lrfcoordinates command.
type options struct {
	file     string
	index    string
	recreate bool
	dryRun   bool
	es       elastic.Config
}

// NewCmd returns the "lrfcoordinates" subcommand.
func NewCmd() *cobra.Command {
	opts := &options{}

	c := &cobra.Command{
		Use:   "lrfcoordinates",
		Short: "Insert LRF Coordinates from CSV file",
		Long: "Reads LRF sites from a CSV file and indexes them into Elasticsearch.\n" +
			"The index is created with a geo_point mapping if it does not exist.\n" +
			"Documents are keyed by LRF code, so re-running updates existing sites.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, opts)
		},
	}

	c.Flags().StringVarP(&opts.file, "file", "f", "", "path to the LRF coordinates input file")
	c.Flags().StringVarP(&opts.index, "index", "i", lrf.DefaultIndex, "Elasticsearch index to insert into")
	c.Flags().BoolVar(&opts.recreate, "recreate", false, "delete and recreate the index before inserting")
	c.Flags().BoolVar(&opts.dryRun, "dry-run", false, "parse the file and print the records without inserting")
	elastic.AddFlags(c.Flags(), &opts.es)
	_ = c.MarkFlagRequired("file")

	return c
}

func run(cmd *cobra.Command, opts *options) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	lrfs, err := lrf.ReadFile(opts.file)
	if err != nil {
		return fmt.Errorf("reading %s: %w", opts.file, err)
	}
	fmt.Fprintf(out, "read %d LRF records from %s\n", len(lrfs), opts.file)

	if opts.dryRun {
		return printTable(cmd, lrfs)
	}

	client, err := elastic.NewClient(opts.es)
	if err != nil {
		return err
	}

	created, err := elastic.EnsureIndex(ctx, client, opts.index, lrf.IndexBody, opts.recreate)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "created index %s\n", opts.index)
	}

	docs := make([]elastic.Doc, len(lrfs))
	for i, l := range lrfs {
		docs[i] = elastic.Doc{ID: l.Code, Source: l.Document()}
	}

	stats, err := elastic.BulkIndex(ctx, client, opts.index, docs)
	fmt.Fprintf(out, "indexed %d documents into %s (%d failed)\n", stats.Indexed, opts.index, stats.Failed)
	return err
}

func printTable(cmd *cobra.Command, lrfs []lrf.LRF) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CODE\tDMA NAME\tCITY\tSTATE\tZIP\tLATITUDE\tLONGITUDE")
	for _, l := range lrfs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%.7f\t%.7f\n",
			l.Code, l.DMAName, l.City, l.State, l.ZipCode, l.Latitude, l.Longitude)
	}
	return w.Flush()
}
