// Command berth is the Berth CLI.
package main

import (
	"os"

	"github.com/dps/berth/internal/cli"
)

var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Execute())
}
