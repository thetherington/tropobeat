package main

import (
	"os"

	"github.com/thetherington/tropobeat/cmd"

	_ "github.com/thetherington/tropobeat/include"
)

func main() {
	if err := cmd.RootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
