// Package insert implements the "tropobeat insert" command group.
package insert

import (
	"github.com/spf13/cobra"

	"github.com/thetherington/tropobeat/cmd/insert/fcctransmitters"
	"github.com/thetherington/tropobeat/cmd/insert/lrfcoordinates"
)

// NewCmd returns the "insert" command with all of its subcommands attached.
func NewCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "insert",
		Short: "Insert data into tropobeat",
	}

	c.AddCommand(lrfcoordinates.NewCmd())
	c.AddCommand(fcctransmitters.NewCmd())

	return c
}
