package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
)

// ---- pull request previews (design.md, Pull request previews) ----

func cmdPreviews() *cobra.Command {
	return &cobra.Command{
		Use:   "previews <app>",
		Short: "List an app's pull request previews",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return previewsCall(daemon.PreviewParams{App: app}, false)
		},
	}
}

func cmdPreviewsOn() *cobra.Command {
	var max int
	c := &cobra.Command{
		Use:   "previews:on <app>",
		Short: "Give each open pull request its own preview app, <app>-pr-<n>",
		Long: "Each open pull request into the app's branch, from a branch of the same GitHub repo, gets\n" +
			"<app>-pr-<n>: built from the PR's latest commit, with its own empty databases, behind the\n" +
			"same access rule as the dev version. A check on the PR links to it; closing the PR removes\n" +
			"it. Pull requests from forks never get one.",
		Example: "  berth previews:on myapp\n  berth previews:on myapp --max 5",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return previewsCall(daemon.PreviewParams{App: app, On: true, Max: max}, true)
		},
	}
	c.Flags().IntVar(&max, "max", 0, "most previews running at once (default 3)")
	return c
}

func cmdPreviewsOff() *cobra.Command {
	return &cobra.Command{
		Use:   "previews:off <app>",
		Short: "Stop making previews and destroy the ones there are",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return previewsCall(daemon.PreviewParams{App: app, Off: true}, true)
		},
	}
}

func previewsCall(p daemon.PreviewParams, changes bool) error {
	var res daemon.PreviewList
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := client().Call(ctx, "previews", p, logsWriter(), &res); err != nil {
		return err
	}
	if jsonOut {
		printJSON(res)
		return nil
	}
	if !res.On {
		if !changes {
			fmt.Printf("Previews are off for %s (berth previews:on %s).\n", res.App, res.App)
		}
		return nil
	}
	if len(res.Previews) == 0 {
		if !changes {
			fmt.Printf("Previews are on for %s, and no pull request has one yet.\n", res.App)
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PR\tAPP\tSTATUS\tREL\tSHA\tURL")
	for _, pv := range res.Previews {
		rel := "-"
		if pv.Release > 0 {
			rel = fmt.Sprintf("v%d", pv.Release)
		}
		fmt.Fprintf(w, "#%d\t%s\t%s\t%s\t%s\t%s\n", pv.PR, pv.App, pv.Status, rel, short(pv.SHA), pv.URL)
	}
	w.Flush()
	return nil
}
