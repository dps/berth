// Command berth is the Berth CLI.
package main

import (
	"os"
	"runtime/debug"

	"github.com/dps/berth/internal/cli"
)

var version = "dev"

func main() {
	if version == "dev" {
		// `go install …@v0.2.0` records the module version instead of ldflags.
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}
	cli.Version = version
	os.Exit(cli.Execute())
}
