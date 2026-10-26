package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/state"
)

func absPath(p string) (string, error) { return filepath.Abs(p) }

func cmdConfigSet() *cobra.Command {
	var noRestart, buildArg bool
	c := &cobra.Command{
		Use:   "config:set <app> KEY=value ...",
		Short: "Set env vars and redeploy",
		Example: "  berth config:set myapp OPENAI_API_KEY=sk-...\n" +
			"  berth config:set myapp BERTH_HEALTHCHECK=/healthz BERTH_MEMORY=1G",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			p := daemon.ConfigParams{App: app, Set: map[string]string{}, NoRestart: noRestart}
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("%q: expected KEY=value", kv)
				}
				if err := state.ValidateEnvKey(k); err != nil {
					return err
				}
				p.Set[k] = v
				if buildArg {
					p.BuildArgs = append(p.BuildArgs, k)
				}
			}
			return configCall(p)
		},
	}
	c.Flags().BoolVar(&noRestart, "no-restart", false, "save without redeploying")
	c.Flags().BoolVar(&buildArg, "build-arg", false, "also pass these vars to the build as --build-arg (rebuilds)")
	return c
}

func configCall(p daemon.ConfigParams) error {
	var res daemon.ReleaseResult
	if err := call("config.set", p, &res); err != nil {
		return err
	}
	if !jsonOut && res.Release != 0 {
		printLive(res)
	}
	return nil
}

func cmdConfigUnset() *cobra.Command {
	var noRestart bool
	c := &cobra.Command{
		Use:   "config:unset <app> KEY ...",
		Short: "Remove env vars and redeploy",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return configCall(daemon.ConfigParams{App: app, Unset: args[1:], NoRestart: noRestart})
		},
	}
	c.Flags().BoolVar(&noRestart, "no-restart", false, "save without redeploying")
	return c
}

func cmdConfigGet() *cobra.Command {
	return &cobra.Command{
		Use:   "config:get <app> KEY",
		Short: "Print one env var",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			env, err := store().Env(app)
			if err != nil {
				return err
			}
			v, ok := env[args[1]]
			if !ok {
				return fmt.Errorf("%s is not set on %s", args[1], app)
			}
			if jsonOut {
				printJSON(map[string]string{args[1]: v})
				return nil
			}
			fmt.Println(v)
			return nil
		},
	}
}

func cmdConfigShow() *cobra.Command {
	var reveal, shell bool
	c := &cobra.Command{
		Use:   "config:show <app>",
		Short: "Show env vars (secrets masked unless --reveal)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			env, err := store().Env(app)
			if err != nil {
				return err
			}
			shown := state.Env{}
			for k, v := range env {
				if !reveal && (state.IsSecretKey(k) || state.IsSecretValue(v)) {
					v = state.Mask(v)
				}
				shown[k] = v
			}
			if jsonOut {
				printJSON(shown)
				return nil
			}
			if shell {
				fmt.Print(shown.Format())
				return nil
			}
			width := 0
			for k := range shown {
				width = max(width, len(k))
			}
			for _, k := range shown.Keys() {
				fmt.Printf("%-*s  %s\n", width+1, k+":", shown[k])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&reveal, "reveal", false, "show secret values")
	c.Flags().BoolVar(&shell, "shell", false, "KEY=value format")
	return c
}

func cmdConfigImport() *cobra.Command {
	var noRestart, replace bool
	c := &cobra.Command{
		Use:   "config:import <app> <.env file|->",
		Short: "Set env vars from a .env file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			f := os.Stdin
			if args[1] != "-" {
				if f, err = os.Open(args[1]); err != nil {
					return err
				}
				defer f.Close()
			}
			env, err := state.ParseEnv(f)
			if err != nil {
				return fmt.Errorf("%s: %w", args[1], err)
			}
			return configCall(daemon.ConfigParams{App: app, Set: env, NoRestart: noRestart, Replace: replace})
		},
	}
	c.Flags().BoolVar(&noRestart, "no-restart", false, "save without redeploying")
	c.Flags().BoolVar(&replace, "replace", false, "remove vars not in the file (BERTH_* settings are kept)")
	return c
}
