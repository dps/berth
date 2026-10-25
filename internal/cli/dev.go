package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/state"
)

// ---- dev versions (design.md, Dev versions) ----

const devRef = "refs/berth/dev"

// gitIn runs git in dir with extra env and returns trimmed stdout.
func gitIn(dir string, env []string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

type snapshot struct {
	SHA, Base, Changes string
	Bytes              int64
}

// takeSnapshot records the working tree (tracked and untracked files,
// .gitignore respected) as a commit on top of HEAD, using a temporary index
// so the developer's index, branch and stash are untouched. A clean tree
// is HEAD itself.
func takeSnapshot(top string) (*snapshot, error) {
	idx, err := os.CreateTemp("", "berth-index-*")
	if err != nil {
		return nil, err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	// Starting from the real index lets git reuse its stat cache.
	if real, err := gitIn(top, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"); err == nil {
		if b, err := os.ReadFile(real); err == nil {
			os.WriteFile(idx.Name(), b, 0o600)
		}
	}
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if _, err := gitIn(top, env, "add", "-A"); err != nil {
		return nil, err
	}
	tree, err := gitIn(top, env, "write-tree")
	if err != nil {
		return nil, err
	}
	snap := &snapshot{}
	if sizes, err := gitIn(top, nil, "ls-tree", "-r", "-l", tree); err == nil {
		for _, line := range strings.Split(sizes, "\n") {
			if f := strings.Fields(line); len(f) >= 4 {
				n, _ := strconv.ParseInt(f[3], 10, 64)
				snap.Bytes += n
			}
		}
	}
	head, _ := gitIn(top, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	snap.Base = head
	if head != "" {
		headTree, _ := gitIn(top, nil, "rev-parse", "HEAD^{tree}")
		if headTree == tree {
			snap.SHA = head
			return snap, nil
		}
		snap.Changes, _ = gitIn(top, nil, "diff-tree", "--shortstat", headTree, tree)
	} else {
		snap.Changes = "first snapshot"
	}
	base := "no commits"
	args := []string{"commit-tree", tree}
	if head != "" {
		base = head[:7]
		args = append(args, "-p", head)
	}
	args = append(args, "-m", fmt.Sprintf("berth dev snapshot: %s + %s", base, snap.Changes))
	who := []string{"GIT_AUTHOR_NAME=berth dev:push", "GIT_AUTHOR_EMAIL=berth@localhost", "GIT_COMMITTER_NAME=berth dev:push", "GIT_COMMITTER_EMAIL=berth@localhost"}
	if snap.SHA, err = gitIn(top, who, args...); err != nil {
		return nil, err
	}
	return snap, nil
}

// devTarget is where a snapshot goes: this Mac's mirror, or another Mac
// over the repo's `berth` SSH remote.
type devTarget struct {
	app    string
	local  bool
	push   string // git URL or path to push to
	sshDst string // user@host, for remote deploys
	port   string
}

func findDevTarget(top, appName string) (*devTarget, error) {
	// This Mac runs Berth: push straight into the app's mirror.
	if _, err := store().Config(); err == nil {
		name := appName
		if name == "" {
			name = matchApp(top)
		}
		if name != "" {
			a, err := store().App(name)
			if err != nil {
				return nil, err
			}
			if a.DevOf != "" {
				name = a.DevOf
			}
			return &devTarget{app: name, local: true, push: store().Path("apps", name, "repo.git")}, nil
		}
		if appName != "" {
			return nil, fmt.Errorf("no app named %q", appName)
		}
	}
	// Another Mac: the `berth` remote that `berth git:remote` adds.
	raw, err := gitIn(top, nil, "remote", "get-url", "berth")
	if err != nil {
		return nil, fmt.Errorf("which app? Run `berth dev:push <app>` on the Berth Mac, or add a `berth` remote there with `berth git:remote <app> --host <this machine's view of the Mac>`")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ssh" {
		return nil, fmt.Errorf("the berth remote %q isn't an ssh:// URL (re-add it with `berth git:remote`)", raw)
	}
	name := filepath.Base(filepath.Dir(u.Path))
	if filepath.Base(u.Path) != "repo.git" || state.ValidateAppName(name) != nil {
		return nil, fmt.Errorf("the berth remote %q doesn't point at a Berth app", raw)
	}
	if appName != "" && appName != name {
		return nil, fmt.Errorf("the berth remote points at %s, not %s", name, appName)
	}
	dst := u.Host
	if u.User != nil {
		dst = u.User.Username() + "@" + u.Hostname()
	}
	return &devTarget{app: name, push: raw, sshDst: dst, port: u.Port()}, nil
}

// matchApp finds the app whose repo is this checkout: by the berth remote's
// path, then by origin.
func matchApp(top string) string {
	apps, _ := store().Apps()
	if raw, err := gitIn(top, nil, "remote", "get-url", "berth"); err == nil {
		if u, err := url.Parse(raw); err == nil && filepath.Base(u.Path) == "repo.git" {
			name := filepath.Base(filepath.Dir(u.Path))
			if store().AppExists(name) {
				return name
			}
		}
	}
	origin, _ := gitIn(top, nil, "remote", "get-url", "origin")
	for _, a := range apps {
		if a.DevOf != "" || a.Repo == "" {
			continue
		}
		if (origin != "" && normalizeRepo(origin) == a.Repo) || a.Repo == top {
			return a.Name
		}
	}
	return ""
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func cmdDevPush() *cobra.Command {
	c := &cobra.Command{
		Use:   "dev:push [app]",
		Short: "Deploy this working tree (uncommitted changes too) to https://<app>-dev.<domain>",
		Long: "Snapshots the repo exactly as it is on disk (tracked and untracked files; .gitignore respected),\n" +
			"without touching your branch, index or stash, and deploys it to the app's dev version.\n" +
			"Exit codes: 0 live, 1 build failed, 2 the app failed to start.",
		Example: "  berth dev:push            # in the app's repo\n  berth dev:push chirp --json",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			top, err := gitIn(".", nil, "rev-parse", "--show-toplevel")
			if err != nil {
				return fmt.Errorf("dev:push runs inside the app's git repo")
			}
			appName := ""
			if len(args) == 1 {
				appName = args[0]
			}
			t, err := findDevTarget(top, appName)
			if err != nil {
				return err
			}
			logs := logsWriter()
			snap, err := takeSnapshot(top)
			if err != nil {
				return err
			}
			if snap.Changes == "" {
				fmt.Fprintf(logs, "-----> Snapshot of %s: clean, deploying HEAD %s\n", t.app, short(snap.SHA))
			} else {
				fmt.Fprintf(logs, "-----> Snapshot of %s: %s on %s\n", t.app, snap.Changes, short(snap.Base))
			}
			if snap.Bytes > 100<<20 {
				fmt.Fprintf(logs, "       warning: the snapshot is %d MB; is a build folder or node_modules missing from .gitignore?\n", snap.Bytes>>20)
			}
			push := exec.Command("git", "push", "--quiet", "--force", "--no-verify", t.push, snap.SHA+":"+devRef)
			push.Dir = top
			push.Stdout, push.Stderr = logs, logs
			if err := push.Run(); err != nil {
				return fmt.Errorf("pushing the snapshot to %s failed", t.push)
			}
			p := daemon.DevDeployParams{App: t.app, Ref: snap.SHA, Base: snap.Base, Changes: snap.Changes}
			var res daemon.DevDeployResult
			if t.local {
				if _, err := client().Call(cmd.Context(), "dev.deploy", p, logs, &res); err != nil {
					return err
				}
			} else if err := remoteDevDeploy(t, p, &res); err != nil {
				return err
			}
			return reportDev(res)
		},
	}
	return c
}

// remoteDevDeploy runs `berth dev:deploy` on the Berth Mac over SSH; its
// progress streams to stderr and the JSON result comes back on stdout.
func remoteDevDeploy(t *devTarget, p daemon.DevDeployParams, res *daemon.DevDeployResult) error {
	remote := fmt.Sprintf("PATH=/opt/homebrew/bin:/usr/local/bin:$PATH berth dev:deploy %s --ref %s --base %s --changes %s --json",
		shellQuote(p.App), shellQuote(p.Ref), shellQuote(p.Base), shellQuote(p.Changes))
	args := []string{}
	if t.port != "" {
		args = append(args, "-p", t.port)
	}
	args = append(args, t.sshDst, remote)
	c := exec.Command("ssh", args...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, os.Stderr
	err := c.Run()
	if jerr := json.Unmarshal(out.Bytes(), res); jerr != nil || res.Status == "" {
		if err != nil {
			return fmt.Errorf("ssh %s: %v %s", t.sshDst, err, strings.TrimSpace(out.String()))
		}
		return fmt.Errorf("unexpected reply from %s: %s", t.sshDst, strings.TrimSpace(out.String()))
	}
	return nil
}

// reportDev prints the result and maps it to the exit code.
func reportDev(res daemon.DevDeployResult) error {
	code := 0
	if res.Status != "live" {
		code = 1
		if res.Stage == "start" {
			code = 2
		}
	}
	if jsonOut {
		printJSON(res)
	} else if code == 0 {
		fmt.Printf("=====> %s v%d is live at %s\n", res.Dev, res.Release, res.URL)
	} else {
		what := "build failed"
		if code == 2 {
			what = "the app failed to start"
		}
		msg := res.Error
		if !strings.HasPrefix(msg, what) {
			msg = what + ": " + msg
		}
		fmt.Fprintf(os.Stderr, " !     %s v%d: %s\n", res.Dev, res.Release, msg)
		fmt.Fprintf(os.Stderr, " !     see `berth logs %s --build` and `berth logs %s`\n", res.Dev, res.Dev)
	}
	if code != 0 {
		return &silentExit{code: code}
	}
	return nil
}

// cmdDevDeploy is the server half of a remote dev:push.
func cmdDevDeploy() *cobra.Command {
	var p daemon.DevDeployParams
	c := &cobra.Command{
		Use:    "dev:deploy <app>",
		Short:  "Deploy a pushed snapshot to the app's dev version (used by dev:push over SSH)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			p.App = app
			var res daemon.DevDeployResult
			if _, err := client().Call(cmd.Context(), "dev.deploy", p, logsWriter(), &res); err != nil {
				return err
			}
			return reportDev(res)
		},
	}
	c.Flags().StringVar(&p.Ref, "ref", "", "snapshot commit (default: the last one pushed)")
	c.Flags().StringVar(&p.Base, "base", "", "commit the snapshot is based on")
	c.Flags().StringVar(&p.Changes, "changes", "", "summary of uncommitted changes")
	return c
}

func parentArg(args []string) (string, error) {
	app, err := appArg(args)
	if err != nil {
		return "", err
	}
	if a, err := store().App(app); err == nil && a.DevOf != "" {
		return a.DevOf, nil
	}
	return app, nil
}

func cmdDevToken() *cobra.Command {
	var as string
	var revoke bool
	c := &cobra.Command{
		Use:   "dev:token <app>",
		Short: "A bearer token that gets agents past the dev version's sign-in",
		Example: "  berth dev:token chirp --as test@example.com\n" +
			"  curl -H \"Authorization: Bearer $BERTH_DEV_TOKEN\" https://chirp-dev.example.com/api/me\n" +
			"  berth dev:token chirp --revoke",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := parentArg(args)
			if err != nil {
				return err
			}
			var res map[string]any
			if _, err := client().Call(cmd.Context(), "dev.token", daemon.DevParams{App: app, As: as, Revoke: revoke}, logsWriter(), &res); err != nil {
				return err
			}
			if jsonOut {
				printJSON(res)
				return nil
			}
			if tok, _ := res["token"].(string); tok != "" {
				fmt.Fprintf(os.Stderr, "Shown once. Keep it out of git (an env var or a gitignored .env):\n")
				fmt.Printf("export BERTH_DEV_TOKEN=%s\n", tok)
			}
			return nil
		},
	}
	c.Flags().StringVar(&as, "as", "", "requests with the token are signed in as this (pretend) email")
	c.Flags().BoolVar(&revoke, "revoke", false, "revoke every dev token for the app")
	return c
}

func cmdDevReset() *cobra.Command {
	return &cobra.Command{
		Use:   "dev:reset <app>",
		Short: "Delete the dev version and its data; the next dev:push starts fresh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := parentArg(args)
			if err != nil {
				return err
			}
			return call("dev.reset", daemon.DevParams{App: app}, nil)
		},
	}
}

func cmdDevDBCopy() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "dev:db:copy <app>",
		Short: "Replace the dev database with a copy of production's",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := parentArg(args)
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Replace %s's dev database with a copy of production's?", app)) {
				return fmt.Errorf("cancelled")
			}
			return call("dev.dbcopy", daemon.DevParams{App: app}, nil)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask")
	return c
}

func cmdDevOnOff(off bool) *cobra.Command {
	use, short := "dev:on <app>", "Give the app a dev address again"
	if off {
		use, short = "dev:off <app>", "Remove the app's dev version and its dev address"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := parentArg(args)
			if err != nil {
				return err
			}
			return call("dev.set", daemon.DevParams{App: app, Off: off}, nil)
		},
	}
}

func cmdDevAgents() *cobra.Command {
	return &cobra.Command{
		Use:   "dev:agents <app>",
		Short: "Print instructions for coding agents (paste into AGENTS.md or CLAUDE.md)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := parentArg(args)
			if err != nil {
				return err
			}
			cfg, err := store().Config()
			if err != nil {
				return err
			}
			a, err := store().App(app)
			if err != nil {
				return err
			}
			dev := state.DevName(app)
			devApp := &state.App{Name: dev, Private: a.Private}
			urls := daemon.AppURLs(cfg, devApp)
			u := "(no URL)"
			if len(urls) > 0 {
				u = urls[0]
			}
			fmt.Printf(`## Testing changes on Berth

This app runs on Berth. Production (%[1]s) deploys only from commits pushed to
its branch. To try changes before committing, use its dev version:

- Deploy the working tree, uncommitted changes included: `+"`berth dev:push`"+`
  (add `+"`--json`"+` for a structured result). Exit code 0 = live, 1 = build failed,
  2 = the app failed to start; build output and app logs are printed on failure.
- Dev URL: %[2]s
  It needs a token: `+"`curl -H \"Authorization: Bearer $BERTH_DEV_TOKEN\" %[2]s/...`"+`.
  If BERTH_DEV_TOKEN isn't set, ask the user to run
  `+"`berth dev:token %[1]s --as test@example.com`"+` (requests are then signed in as that test user).
- Logs: `+"`berth logs %[3]s -n 200`"+` (`+"`--build`"+` for the last build). One-off commands
  such as migrations: `+"`berth run %[3]s <command>`"+`. Config: `+"`berth config:set %[3]s KEY=value`"+`.
- The dev version has its own database, separate from production's.
  `+"`berth dev:reset %[1]s`"+` starts it over; `+"`berth dev:db:copy %[1]s -y`"+` loads a copy of production's data.
- Never push to the production branch just to test something.
`, app, u, dev)
			return nil
		},
	}
}
