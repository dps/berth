// Package cli is the `berth` command. Every command maps to a Dokku
// concept; mutating commands go through berthd, read-only ones read
// ~/.berth directly so they work when the daemon is down.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/rpc"
	"github.com/dps/berth/internal/state"
)

var Version = "dev"

var jsonOut bool

func store() *state.Store { return state.Open() }

func client() *rpc.Client { return rpc.NewClient(store().SocketPath()) }

// logsWriter is where streamed progress goes: stdout normally, stderr with
// --json so stdout stays machine-readable.
func logsWriter() io.Writer {
	if jsonOut {
		return os.Stderr
	}
	return os.Stdout
}

// call invokes a daemon method, streaming progress, and decodes the result.
func call(method string, params any, out any) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	raw, err := client().Call(ctx, method, params, logsWriter(), out)
	if err != nil {
		return err
	}
	if jsonOut {
		printJSON(json.RawMessage(raw))
	}
	return nil
}

func printJSON(v any) {
	if raw, ok := v.(json.RawMessage); ok && len(raw) == 0 {
		v = map[string]any{}
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// exitError carries a hint printed under the error.
type exitError struct {
	msg  string
	hint string
}

func (e *exitError) Error() string { return e.msg }

func hintErr(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &exitError{msg: err.Error(), hint: hint}
}

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "berth",
		Short:         "A Dokku for Mac minis: git push and your app is live",
		Long:          "Berth turns an Apple Silicon Mac into a small PaaS: push to GitHub and the app is live at https://<app>.<your domain>.\n\nStart with `berth init --domain example.com`, then `berth create <app> --repo owner/name`.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	root.AddGroup(
		&cobra.Group{ID: "daily", Title: "Daily commands:"},
		&cobra.Group{ID: "apps", Title: "Apps:"},
		&cobra.Group{ID: "services", Title: "Services:"},
		&cobra.Group{ID: "ops", Title: "Operations:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("daily", cmdDeploy(), cmdRollback(), cmdLogs(), cmdStatus(), cmdConfigSet(), cmdRestart())
	add("apps", cmdCreate(), cmdDestroy(), cmdStop(), cmdStart(), cmdReleases(),
		cmdConfigGet(), cmdConfigUnset(), cmdConfigShow(), cmdConfigImport(),
		cmdDomainsAdd(), cmdDomainsRemove(), cmdDomainsList(),
		cmdAuthSet(), cmdAuthUnset(),
		cmdStorageMount(), cmdStorageUnmount(), cmdStorageList(),
		cmdRun(), cmdExec(), cmdDockerfile(), cmdGitRemote(), cmdSetKey())
	add("services", serviceCommands("pg")...)
	add("services", serviceCommands("redis")...)
	add("ops", cmdInit(), cmdDoctor(), cmdEvents(), cmdGC(), cmdBackup(), cmdRestore(),
		cmdNotifySet(), cmdReboot(), cmdReload(), cmdTrustCA(), cmdUninstall())
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := NewRoot()
	if err := root.Execute(); err != nil {
		if jsonOut {
			b, _ := json.Marshal(map[string]string{"error": err.Error()})
			fmt.Fprintln(os.Stdout, string(b))
			return 1
		}
		fmt.Fprintf(os.Stderr, " !     %s\n", err)
		var ee *exitError
		switch {
		case errors.As(err, &ee) && ee.hint != "":
			fmt.Fprintf(os.Stderr, " !     %s\n", ee.hint)
		case errors.Is(err, rpc.ErrDaemonDown), errors.Is(err, state.ErrNotInitialized):
		case strings.Contains(err.Error(), "deploy") || strings.Contains(err.Error(), "health"):
			fmt.Fprintln(os.Stderr, " !     see `berth logs <app> --build` and `berth logs <app>`")
		}
		return 1
	}
	return 0
}

// appArg validates the app name argument exists.
func appArg(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("missing app name")
	}
	if !store().AppExists(args[0]) {
		return "", hintErr(fmt.Errorf("no app named %q", args[0]), "`berth status` lists apps")
	}
	return args[0], nil
}
