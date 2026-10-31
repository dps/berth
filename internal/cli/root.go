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
	code int // process exit code; 0 means 1
}

func (e *exitError) Error() string { return e.msg }

// silentExit ends the command with a code after it has printed its own
// output (dev:push --json prints a result even when the deploy failed).
type silentExit struct{ code int }

func (e *silentExit) Error() string { return fmt.Sprintf("exit %d", e.code) }

func hintErr(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &exitError{msg: err.Error(), hint: hint}
}

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "berth",
		Short:         "A dead simple PaaS for Mac minis: git push and your app is live",
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
	add("apps", cmdNew(), cmdCreate(), cmdDestroy(), cmdStop(), cmdStart(), cmdReleases(),
		cmdConfigGet(), cmdConfigUnset(), cmdConfigShow(), cmdConfigImport(),
		cmdDomainsAdd(), cmdDomainsRemove(), cmdDomainsList(),
		cmdAuthSet(), cmdAuthUnset(), cmdAccess(),
		cmdStorageMount(), cmdStorageUnmount(), cmdStorageList(),
		cmdRun(), cmdExec(), cmdScale(), cmdCron(), cmdCronRun(), cmdDockerfile(), cmdGitRemote(), cmdSetKey(), cmdSetRepo())
	root.AddGroup(&cobra.Group{ID: "dev", Title: "Dev versions and previews (<app>-dev, <app>-pr-<n>; app commands work on them too):"})
	add("dev", cmdDevPush(), cmdDevToken(), cmdDevDBCopy(), cmdDevReset(), cmdDevAgents(), cmdDevOnOff(true), cmdDevOnOff(false), cmdDevDeploy(),
		cmdPreviews(), cmdPreviewsOn(), cmdPreviewsOff())
	root.AddGroup(&cobra.Group{ID: "signin", Title: "Sign-in and admin page:"})
	add("signin", cmdAuthSetup(), cmdAuthStatus(), cmdAuthGroup(), cmdAuthSessions(), cmdAuthRevoke())
	add("signin", cmdAdmin(), cmdAdminEnable(), cmdAdminDisable())
	root.AddGroup(&cobra.Group{ID: "ai", Title: "AI gateway:"})
	add("ai", cmdAISetup(), cmdAIStatus(), cmdAIModels(), cmdAIKey(), cmdAIAlias(), cmdAIPull(), cmdAIEnable(), cmdAIDisable(), cmdAIUsage())
	add("services", serviceCommands("pg")...)
	add("services", serviceCommands("redis")...)
	add("services", sqliteCommands()...)
	add("ops", cmdInit(), cmdDoctor(), cmdEvents(), cmdGC(), cmdBackup(), cmdRestore(),
		cmdOffsiteSetup(), cmdOffsiteStatus(), cmdOffsiteRun(), cmdOffsiteGet(), cmdOffsiteKey(), cmdOffsiteOff(),
		cmdNotifySet(), cmdOnVisitSet(), cmdReboot(), cmdReload(), cmdCFLogin(), cmdTrustCA(), cmdUpgrade(), cmdUninstall())
	root.AddCommand(cmdInfo())
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := NewRoot()
	if code, ok := forward(root, os.Args[1:]); ok {
		return code
	}
	// Run over SSH from another machine's CLI: say if the versions differ.
	if w := versionSkew(os.Getenv("BERTH_CLIENT_VERSION"), Version); w != "" {
		fmt.Fprintf(os.Stderr, " !     %s\n", w)
	}
	if err := root.Execute(); err != nil {
		return printErr(err)
	}
	return 0
}

// printErr prints a command's error and returns its exit code.
func printErr(err error) int {
	var silent *silentExit
	if errors.As(err, &silent) {
		return silent.code
	}
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
	if ee != nil && ee.code != 0 {
		return ee.code
	}
	return 1
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
