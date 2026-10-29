package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
)

// ---- workers and cron (design.md, Workers and cron) ----

func cmdScale() *cobra.Command {
	return &cobra.Command{
		Use:   "ps:scale <app> <type>=<n>...",
		Short: "Run n of a Procfile worker type (0 stops it)",
		Long: "Process types in the Procfile other than web and release (worker: node worker.js) run as\n" +
			"workers inside the app's container, one of each by default, restarted if they exit.",
		Example: "  berth ps:scale myapp worker=2\n  berth ps:scale myapp clock=0",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			for _, kv := range args[1:] {
				typ, ns, ok := strings.Cut(kv, "=")
				n, err := strconv.Atoi(ns)
				if !ok || err != nil {
					return fmt.Errorf("%q: write <type>=<n>, like worker=2", kv)
				}
				if err := call("ps.scale", daemon.ScaleParams{App: app, Type: typ, Count: n}, nil); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func cmdCron() *cobra.Command {
	return &cobra.Command{
		Use:   "cron <app>",
		Short: "List an app's scheduled jobs (app.json's cron), next and last runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			var res daemon.CronList
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := client().Call(ctx, "cron.list", daemon.CronParams{App: app}, nil, &res); err != nil {
				return err
			}
			if jsonOut {
				printJSON(res)
				return nil
			}
			if len(res.Jobs) == 0 {
				fmt.Printf("%s has no scheduled jobs. Add them to app.json in the repo:\n", app)
				fmt.Println(`  {"cron": [{"command": "npm run digest", "schedule": "0 7 * * *"}]}`)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "#\tSCHEDULE\tCOMMAND\tNEXT\tLAST RUN")
			for _, j := range res.Jobs {
				next := "-"
				if !j.Next.IsZero() && res.Paused == "" {
					next = j.Next.Format("Mon 15:04")
				}
				last := "never"
				switch {
				case j.Running:
					last = "running now"
				case j.Last != nil && j.Last.Error != "":
					last = fmt.Sprintf("failed %s: %s", ago(j.Last.Started), j.Last.Error)
				case j.Last != nil:
					last = fmt.Sprintf("ok %s (%s)", ago(j.Last.Started), time.Duration(j.Last.Secs*float64(time.Second)).Round(100*time.Millisecond))
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", j.Index, j.Schedule, j.Command, next, last)
			}
			w.Flush()
			if res.Paused != "" {
				fmt.Printf("\nSchedules are paused: %s.\n", res.Paused)
			}
			fmt.Printf("\nOutput: berth logs %s --cron. Times are this Mac's (%s).\n", app, time.Now().Format("MST"))
			return nil
		},
	}
}

func cmdCronRun() *cobra.Command {
	return &cobra.Command{
		Use:     "cron:run <app> <n>",
		Short:   "Run scheduled job n now and wait for it (berth cron lists them)",
		Example: "  berth cron:run myapp 1\n  berth cron:run myapp-dev 1    # try it on the dev version",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("the job number, from `berth cron %s`", app)
			}
			return call("cron.run", daemon.CronParams{App: app, Index: n}, nil)
		},
	}
}
