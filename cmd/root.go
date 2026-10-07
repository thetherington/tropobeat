package cmd

import (
	"github.com/thetherington/tropobeat/beater"
	"github.com/thetherington/tropobeat/cmd/insert"

	cmd "github.com/elastic/beats/v7/libbeat/cmd"
	"github.com/elastic/beats/v7/libbeat/cmd/instance"
)

// Name of this beat
var Name = "tropobeat"

// RootCmd to handle beats cli
var RootCmd = cmd.GenRootCmdWithSettings(beater.New, instance.Settings{Name: Name})

func init() {
	RootCmd.AddCommand(insert.NewCmd())
}
