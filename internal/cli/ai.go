package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/ai"
	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/runtime"
)

// ---- AI gateway (design.md, AI gateway) ----

func ollamaBase() string {
	ac, err := store().AIConfig()
	if err != nil || ac.OllamaURL == "" {
		return ai.DefaultOllamaURL
	}
	return strings.TrimSuffix(ac.OllamaURL, "/")
}

func ollamaUp(base string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := ai.OllamaVersion(ctx, base)
	return err == nil
}

// startOllama installs Ollama with Homebrew if needed and starts it, with
// the user's say-so for each step.
func startOllama(base string, yes bool) error {
	if ollamaUp(base) {
		return nil
	}
	if base != ai.DefaultOllamaURL {
		return fmt.Errorf("Ollama isn't answering at %s (ollama_url in ~/.berth/ai/config.json)", base)
	}
	if _, err := os.Stat("/Applications/Ollama.app"); err == nil {
		fmt.Println("-----> Starting Ollama.app")
		exec.Command("open", "-ga", "Ollama").Run()
	} else {
		bin := runtime.LookBin("ollama")
		brew := runtime.LookBin("brew")
		if _, err := exec.LookPath(bin); err != nil {
			if !yes && !confirm("Ollama runs local models on this Mac's GPU. Install it with Homebrew (brew install ollama)?") {
				return fmt.Errorf("install Ollama (https://ollama.com or brew install ollama), then run berth ai:setup again; or skip local models with --cloud-only")
			}
			if err := run(brew, "install", "ollama"); err != nil {
				return err
			}
		}
		if !yes && !confirm("Start Ollama now and whenever this Mac starts (brew services start ollama)?") {
			return fmt.Errorf("start Ollama yourself (ollama serve), then run berth ai:setup again")
		}
		if err := run(brew, "services", "start", "ollama"); err != nil {
			return err
		}
	}
	fmt.Print("-----> Waiting for Ollama")
	for i := 0; i < 60; i++ {
		if ollamaUp(base) {
			fmt.Println(" ✓")
			return nil
		}
		fmt.Print(".")
		time.Sleep(time.Second)
	}
	fmt.Println()
	return fmt.Errorf("Ollama didn't start answering at %s", base)
}

func run(name string, args ...string) error {
	fmt.Printf("-----> %s %s\n", name, strings.Join(args, " "))
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func memBytes() uint64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// pullModel downloads a model unless Ollama already has it.
func pullModel(base, model string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if have, err := ai.OllamaModels(ctx, base); err == nil {
		for _, m := range have {
			if m.Name == model || m.Name == model+":latest" {
				fmt.Printf("-----> %s is already downloaded\n", model)
				return nil
			}
		}
	}
	fmt.Printf("-----> Downloading %s\n", model)
	last := ""
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	err := ai.OllamaPull(ctx, base, model, func(p ai.PullProgress) {
		line := p.Status
		if p.Total > 0 {
			line = fmt.Sprintf("%s %s / %s", p.Status, deploy.HumanBytes(p.Completed), deploy.HumanBytes(p.Total))
		}
		switch {
		case tty:
			fmt.Printf("\r       %-60s", line)
		case p.Status != last:
			fmt.Printf("       %s\n", p.Status)
		}
		last = p.Status
	})
	if tty {
		fmt.Println()
	}
	return err
}

func cmdAISetup() *cobra.Command {
	var model string
	var yes, cloudOnly bool
	c := &cobra.Command{
		Use:   "ai:setup",
		Short: "Set up the AI gateway: Ollama for local models, plus any cloud keys you add",
		Long: "Installs and starts Ollama (asking first), downloads a local model sized to this Mac's\n" +
			"memory (qwen3:8b from 16 GB, qwen3:4b below), and points the \"default\" alias at it.\n" +
			"Cloud providers are added with berth ai:key; apps get access with berth ai:enable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.AISetupParams{}
			if !cloudOnly {
				base := ollamaBase()
				if err := startOllama(base, yes); err != nil {
					return err
				}
				if model == "" {
					model = ai.RecommendedModel(memBytes())
					fmt.Printf("-----> This Mac has %s of memory: using %s (choose another with --model)\n", deploy.HumanBytes(int64(memBytes())), model)
				}
				if err := pullModel(base, model); err != nil {
					return err
				}
				p.Model = model
			}
			if err := call("ai.setup", p, nil); err != nil {
				return err
			}
			if !jsonOut {
				fmt.Println("\nNext:\n  berth ai:enable <app>            # give an app access (it gets OPENAI_BASE_URL and a token)\n" +
					"  berth ai:key anthropic           # add a cloud provider (openai, gemini, openrouter, groq, mistral)\n" +
					"  berth ai:alias smart anthropic/<model>")
			}
			return nil
		},
	}
	c.Flags().StringVar(&model, "model", "", "local model for the default alias (Ollama name, e.g. qwen3:8b)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "install and start Ollama without asking")
	c.Flags().BoolVar(&cloudOnly, "cloud-only", false, "no local models: skip Ollama")
	return c
}

func aiStatus() (*daemon.AIStatus, error) {
	var st daemon.AIStatus
	if err := call("ai.status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func cmdAIStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "ai:status",
		Short: "Show the AI gateway: local models, providers, aliases and which apps use it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := aiStatus()
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(st)
				return nil
			}
			if !st.Configured {
				fmt.Println("AI gateway: not set up (berth ai:setup)")
				return nil
			}
			fmt.Printf("Gateway:  %s (as apps see it)\n", st.URL)
			if st.OllamaError != "" {
				fmt.Printf("Ollama:   not answering at %s\n", st.OllamaURL)
			} else {
				fmt.Printf("Ollama:   %s at %s\n", st.OllamaVersion, st.OllamaURL)
				for _, m := range st.Loaded {
					fmt.Printf("          %s in memory (%s, unloads in %s if idle)\n", m.Name, deploy.HumanBytes(m.Size), time.Until(m.ExpiresAt).Round(time.Second))
				}
			}
			printAIModels(st)
			fmt.Println("\nApps:")
			if len(st.Apps) == 0 {
				fmt.Println("  none (berth ai:enable <app>)")
			}
			for _, a := range st.Apps {
				budget := "no cap"
				if a.MonthlyTokens > 0 {
					budget = fmt.Sprintf("of %s", tokens(a.MonthlyTokens))
				}
				models := "any model"
				if len(a.Models) > 0 {
					models = strings.Join(a.Models, ", ")
				}
				fmt.Printf("  %-16s %s tokens this month (%s) · %s\n", a.App, tokens(a.UsedTokens), budget, models)
			}
			return nil
		},
	}
}

func printAIModels(st *daemon.AIStatus) {
	fmt.Println("\nAliases:")
	if len(st.Aliases) == 0 {
		fmt.Println("  none (berth ai:alias <name> <provider/model>)")
	}
	for _, name := range sortedKeys(st.Aliases) {
		fmt.Printf("  %-12s → %s\n", name, st.Aliases[name])
	}
	fmt.Println("\nLocal models (local/<name>):")
	if len(st.Local) == 0 {
		fmt.Println("  none (berth ai:pull <model>)")
	}
	for _, m := range st.Local {
		fmt.Printf("  %-24s %s\n", m.Name, deploy.HumanBytes(m.Size))
	}
	fmt.Println("\nCloud providers (<provider>/<model>):")
	if len(st.Providers) == 0 {
		fmt.Println("  none (berth ai:key <provider>)")
	}
	for _, p := range st.Providers {
		key := "key set"
		if !p.HasKey {
			key = "no key"
		}
		fmt.Printf("  %-12s %s (%s)\n", p.Name, p.URL, key)
	}
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func cmdAIModels() *cobra.Command {
	return &cobra.Command{
		Use:   "ai:models",
		Short: "List what apps can ask for: aliases, local models and cloud providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := aiStatus()
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(map[string]any{"aliases": st.Aliases, "local": st.Local, "providers": st.Providers})
				return nil
			}
			printAIModels(st)
			return nil
		},
	}
}

func cmdAIKey() *cobra.Command {
	var url string
	var remove bool
	c := &cobra.Command{
		Use:   "ai:key <provider> [key]",
		Short: "Save a cloud provider's API key in Berth (apps never see it)",
		Example: "  berth ai:key anthropic                       # prompts for the key\n" +
			"  berth ai:key openai sk-...\n" +
			"  berth ai:key together --url https://api.together.xyz/v1   # any OpenAI-compatible service\n" +
			"  berth ai:key lmstudio --url http://studio.local:1234/v1     # no key needed\n" +
			"  berth ai:key openai --remove",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.AIKeyParams{Provider: args[0], URL: url, Remove: remove}
			if len(args) == 2 {
				p.Key = args[1]
			} else if !remove && url == "" {
				fmt.Printf("%s API key (hidden): ", p.Provider)
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
			return call("ai.key", p, nil)
		},
	}
	c.Flags().StringVar(&url, "url", "", "OpenAI-compatible base URL, for providers Berth doesn't know")
	c.Flags().BoolVar(&remove, "remove", false, "forget the provider and its key")
	return c
}

func cmdAIAlias() *cobra.Command {
	var remove bool
	c := &cobra.Command{
		Use:   "ai:alias [name] [provider/model]",
		Short: "Point a name apps ask for at a model; changes apply at once, no redeploy",
		Example: "  berth ai:alias smart anthropic/claude-sonnet-5-5\n" +
			"  berth ai:alias default local/qwen3:8b\n" +
			"  berth ai:alias fast groq/llama-3.3-70b-versatile\n" +
			"  berth ai:alias smart --remove",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				st, err := aiStatus()
				if err != nil {
					return err
				}
				if jsonOut {
					printJSON(st.Aliases)
					return nil
				}
				for _, name := range sortedKeys(st.Aliases) {
					fmt.Printf("%-12s → %s\n", name, st.Aliases[name])
				}
				return nil
			}
			p := daemon.AIAliasParams{Name: args[0], Remove: remove}
			if len(args) == 2 {
				p.Model = args[1]
			} else if !remove {
				return fmt.Errorf("usage: berth ai:alias <name> <provider/model>  (or --remove)")
			}
			return call("ai.alias", p, nil)
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, "delete the alias")
	return c
}

func cmdAIPull() *cobra.Command {
	return &cobra.Command{
		Use:     "ai:pull <model>",
		Short:   "Download a local model with Ollama (apps then ask for local/<model>)",
		Example: "  berth ai:pull nomic-embed-text   # embeddings\n  berth ai:pull gemma3:4b",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base := ollamaBase()
			if !ollamaUp(base) {
				return fmt.Errorf("Ollama isn't answering at %s (berth ai:setup)", base)
			}
			if err := pullModel(base, args[0]); err != nil {
				return err
			}
			fmt.Printf("=====> Apps can use it as local/%s (or berth ai:alias <name> local/%s)\n", args[0], args[0])
			return nil
		},
	}
}

func cmdAIEnable() *cobra.Command {
	var p daemon.AIEnableParams
	var models string
	c := &cobra.Command{
		Use:   "ai:enable <app>",
		Short: "Give an app the AI gateway: sets OPENAI_BASE_URL/ANTHROPIC_BASE_URL and its own token",
		Example: "  berth ai:enable chirp\n" +
			"  berth ai:enable chirp --models default,smart --monthly-tokens 2000000 --rpm 60\n" +
			"  berth ai:enable chirp-dev --models 'local/*,smart'   # dev versions get local models only by default",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			p.App = app
			p.Models = nil
			for _, m := range strings.Split(models, ",") {
				if m = strings.TrimSpace(m); m != "" {
					p.Models = append(p.Models, m)
				}
			}
			var res daemon.ReleaseResult
			if err := call("ai.enable", p, &res); err != nil {
				return err
			}
			if !jsonOut && res.Release > 0 {
				printLive(res)
			}
			return nil
		},
	}
	c.Flags().StringVar(&models, "models", "", "allowed models: aliases, provider/model or provider/* (default: any)")
	c.Flags().Int64Var(&p.MonthlyTokens, "monthly-tokens", 0, "cap on input+output tokens per calendar month (0 = none)")
	c.Flags().IntVar(&p.RPM, "rpm", 0, "cap on requests per minute (0 = none)")
	c.Flags().BoolVar(&p.Replace, "replace", false, "replace the app's own OPENAI_API_KEY/ANTHROPIC_API_KEY")
	c.Flags().BoolVar(&p.NoRestart, "no-restart", false, "don't redeploy now")
	return c
}

func cmdAIDisable() *cobra.Command {
	return &cobra.Command{
		Use:   "ai:disable <app>",
		Short: "Revoke an app's AI gateway token and remove its variables",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("ai.disable", daemon.AppParams{App: app}, nil)
		},
	}
}

func cmdAIUsage() *cobra.Command {
	var month string
	c := &cobra.Command{
		Use:   "ai:usage [app]",
		Short: "Requests and tokens per app and model this month (prompts are never logged)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := daemon.AIUsageParams{Month: month}
			if len(args) == 1 {
				p.App = args[0]
			}
			var rows []ai.Row
			if err := call("ai.usage", p, &rows); err != nil {
				return err
			}
			if jsonOut {
				printJSON(rows)
				return nil
			}
			if len(rows) == 0 {
				fmt.Println("No AI requests yet this month.")
				return nil
			}
			fmt.Printf("%-16s %-36s %8s %7s %9s %9s %8s\n", "APP", "MODEL", "REQUESTS", "ERRORS", "IN", "OUT", "AVG")
			for _, r := range rows {
				model := r.Model
				if model == "" {
					model = "(refused)"
				}
				fmt.Printf("%-16s %-36s %8d %7d %9s %9s %7.1fs\n", r.App, model, r.Requests, r.Errors, tokens(r.In), tokens(r.Out), float64(r.AvgMs)/1000)
			}
			return nil
		},
	}
	c.Flags().StringVar(&month, "month", "", "YYYY-MM (default this month)")
	return c
}
