package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/tools"
)

// ---- Tools gateway (design.md, Tools gateway) ----

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func cmdTools() *cobra.Command {
	return &cobra.Command{
		Use:   "tools",
		Short: "Show the tools every app can use, their providers and which apps use them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var st daemon.ToolsStatus
			if err := call("tools.status", nil, &st); err != nil {
				return err
			}
			if jsonOut {
				printJSON(st)
				return nil
			}
			fmt.Printf("%-20s %-24s %s\n", "TOOL", "PROVIDER", "WHAT IT DOES")
			for _, t := range st.Tools {
				via := strings.Join(t.Providers, ", ")
				if via == "" {
					via = "needs a key"
				}
				fmt.Printf("%-20s %-24s %s\n", t.Name, via, t.Summary)
			}
			fmt.Println("\nProviders:")
			for _, p := range st.Providers {
				if p.SignUp == "" {
					continue
				}
				line := "no key: berth tools:key " + p.Name + " (" + p.SignUp + ")"
				if p.HasKey {
					line = fmt.Sprintf("key set; %d calls today", p.Today)
					if p.Budget > 0 {
						line += fmt.Sprintf(" of %d", p.Budget)
					}
				}
				fmt.Printf("  %-12s %s\n", p.Name, line)
			}
			fmt.Println("\nApps:")
			if len(st.Apps) == 0 {
				fmt.Println("  none (berth tools:enable <app>)")
			}
			for _, a := range st.Apps {
				line := fmt.Sprintf("%d calls this month", a.Used)
				if a.MonthlyCalls > 0 {
					line = fmt.Sprintf("%d of %d calls this month", a.Used, a.MonthlyCalls)
				}
				if len(a.Tools) > 0 {
					line += "; " + strings.Join(a.Tools, ", ")
				}
				if a.KeylessOnly {
					line += "; keyless only"
				}
				fmt.Printf("  %-16s %s\n", a.App, line)
			}
			return nil
		},
	}
}

func cmdToolsKey() *cobra.Command {
	var remove bool
	var daily int
	c := &cobra.Command{
		Use:   "tools:key <provider> [key]",
		Short: "Save a provider's API key in Berth, for every app to use (apps never see it)",
		Example: "  berth tools:key exa                     # prompts for the key\n" +
			"  berth tools:key ebay --daily 5000       # share a free tier: at most this many calls a day\n" +
			"  berth tools:key exa --remove",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.ToolsKeyParams{Provider: args[0], Remove: remove}
			if cmd.Flags().Changed("daily") {
				p.Daily = &daily
			}
			if len(args) == 2 {
				p.Key = args[1]
			} else if !remove && p.Daily == nil {
				hint := "API key"
				if prov, ok := tools.FindProvider(p.Provider); ok && prov.KeyHint != "" {
					hint = prov.KeyHint
				}
				fmt.Printf("%s %s (hidden): ", p.Provider, hint)
				if term.IsTerminal(int(os.Stdin.Fd())) {
					b, err := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Println()
					if err != nil {
						return err
					}
					p.Key = strings.TrimSpace(string(b))
				} else {
					p.Key = ask("")
				}
			}
			return call("tools.key", p, nil)
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, "forget the key")
	c.Flags().IntVar(&daily, "daily", 0, "cap on this provider's calls per day across all apps (0 = none)")
	return c
}

func cmdToolsUse() *cobra.Command {
	return &cobra.Command{
		Use:   "tools:use <tool> [provider,provider...]",
		Short: "Choose the providers behind a tool, first choice first; apps switch at once",
		Example: "  berth tools:use web.search exa,serpapi\n" +
			"  berth tools:use web.search                # back to the default order",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.ToolsUseParams{Tool: args[0]}
			if len(args) == 2 {
				p.Providers = splitList(args[1])
			}
			return call("tools.use", p, nil)
		},
	}
}

func cmdToolsEnable() *cobra.Command {
	var p daemon.ToolsEnableParams
	var list string
	c := &cobra.Command{
		Use:   "tools:enable <app>",
		Short: "Give an app the tools gateway: sets BERTH_TOOLS_URL and its own BERTH_TOOLS_KEY",
		Example: "  berth tools:enable chirp\n" +
			"  berth tools:enable chirp --tools 'web.*,weather.forecast' --monthly-calls 10000 --rpm 60\n" +
			"  berth tools:enable chirp-dev --full     # dev versions get keyless tools only by default",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			p.App, p.Tools = app, splitList(list)
			var res daemon.ReleaseResult
			if err := call("tools.enable", p, &res); err != nil {
				return err
			}
			if !jsonOut && res.Release > 0 {
				printLive(res)
			}
			return nil
		},
	}
	c.Flags().StringVar(&list, "tools", "", "allowed tools: names or patterns like web.* (default: all)")
	c.Flags().Int64Var(&p.MonthlyCalls, "monthly-calls", 0, "cap on provider calls per calendar month; cache hits are free (0 = none)")
	c.Flags().IntVar(&p.RPM, "rpm", 0, "cap on calls per minute (0 = none)")
	c.Flags().BoolVar(&p.Full, "full", false, "let a dev version use providers that spend your keys")
	c.Flags().BoolVar(&p.NoRestart, "no-restart", false, "don't redeploy now")
	return c
}

func cmdToolsDisable() *cobra.Command {
	return &cobra.Command{
		Use:   "tools:disable <app>",
		Short: "Revoke an app's tools token and remove its variables",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("tools.disable", daemon.ToolsDisableParams{App: app}, nil)
		},
	}
}

func cmdToolsCall() *cobra.Command {
	return &cobra.Command{
		Use:   "tools:call <tool> [json]",
		Short: "Try a tool from here, as an app would call it",
		Example: "  berth tools:call weather.current '{\"location\":\"Paris\"}'\n" +
			"  berth tools:call web.fetch '{\"url\":\"https://example.com\"}'",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.ToolsCallParams{Tool: args[0]}
			if len(args) == 2 {
				if !json.Valid([]byte(args[1])) {
					return fmt.Errorf("the arguments must be JSON, like '{\"location\":\"Paris\"}'")
				}
				p.Args = json.RawMessage(args[1])
			}
			var res struct {
				Result json.RawMessage `json:"result"`
				Meta   tools.Meta      `json:"meta"`
			}
			if err := call("tools.call", p, &res); err != nil {
				return err
			}
			if jsonOut {
				printJSON(res)
				return nil
			}
			printJSON(res.Result)
			from := res.Meta.Provider
			if res.Meta.Cached {
				from += ", cached"
			}
			fmt.Fprintf(os.Stderr, "(%s)\n", from)
			return nil
		},
	}
}

func cmdToolsDocs() *cobra.Command {
	return &cobra.Command{
		Use:   "tools:docs [app]",
		Short: "Print how to call the tools, as Markdown for a coding agent",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.ToolsDocsParams{}
			if len(args) == 1 {
				p.App = args[0]
			}
			var res struct {
				Markdown string `json:"markdown"`
			}
			if err := call("tools.docs", p, &res); err != nil {
				return err
			}
			fmt.Print(res.Markdown)
			return nil
		},
	}
}

func cmdToolsUsage() *cobra.Command {
	var month string
	c := &cobra.Command{
		Use:   "tools:usage [app]",
		Short: "Calls per app, tool and provider this month (arguments are never logged)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.ToolsUsageParams{Month: month}
			if len(args) == 1 {
				p.App = args[0]
			}
			var rows []tools.Row
			if err := call("tools.usage", p, &rows); err != nil {
				return err
			}
			if jsonOut {
				printJSON(rows)
				return nil
			}
			if len(rows) == 0 {
				fmt.Println("No tool calls yet this month.")
				return nil
			}
			fmt.Printf("%-16s %-20s %-12s %7s %7s %7s %8s\n", "APP", "TOOL", "PROVIDER", "CALLS", "CACHED", "ERRORS", "AVG")
			for _, r := range rows {
				provider := r.Provider
				if provider == "" {
					provider = "(refused)"
				}
				fmt.Printf("%-16s %-20s %-12s %7d %7d %7d %7dms\n", r.App, r.Tool, provider, r.Calls, r.Cached, r.Errors, r.AvgMs)
			}
			return nil
		},
	}
	c.Flags().StringVar(&month, "month", "", "YYYY-MM (default this month)")
	return c
}
