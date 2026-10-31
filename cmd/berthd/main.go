// Command berthd is the Berth daemon, normally run by launchd as
// dev.berth.berthd.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tools"
)

var version = "dev"

func main() {
	// berthd runs itself as the tools gateway (internal/tools).
	if len(os.Args) == 2 && os.Args[1] == "tools" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := tools.Serve(ctx, state.Open()); err != nil {
			slog.Error("tools gateway", "err", err)
			os.Exit(1)
		}
		return
	}
	debug := flag.Bool("debug", os.Getenv("BERTH_DEBUG") != "", "log every external command with its duration")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	daemon.Version = version

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	d := daemon.New(state.Open(), runtime.NewApple())
	if err := d.Run(ctx); err != nil {
		slog.Error("berthd", "err", err)
		os.Exit(1)
	}
}
