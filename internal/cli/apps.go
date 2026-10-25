package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/state"
)

func cmdCreate() *cobra.Command {
	var p daemon.CreateParams
	c := &cobra.Command{
		Use:   "create <app> [--repo owner/name]",
		Short: "Register an app, its webhook and hostname, and deploy it",
		Example: "  berth create myapp --repo me/myapp\n" +
			"  berth create api --repo me/mono --path services/api\n" +
			"  berth create myapp-staging --repo me/myapp --branch staging",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.Name = args[0]
			if err := state.ValidateAppName(p.Name); err != nil {
				return err
			}
			if p.Repo != "" {
				p.Repo = normalizeRepo(p.Repo)
			}
			var res daemon.CreateResult
			if err := call("apps.create", p, &res); err != nil {
				return err
			}
			if !jsonOut && len(res.URLs) > 0 {
				if p.NoDeploy {
					fmt.Printf("\n%s will be at %s after `berth deploy %s`\n", p.Name, res.URLs[0], p.Name)
				} else {
					fmt.Printf("\n%s is live at %s\n", p.Name, res.URLs[0])
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&p.Repo, "repo", "", "GitHub owner/name, or any git URL or local path")
	c.Flags().StringVar(&p.Branch, "branch", "main", "branch to deploy")
	c.Flags().StringVar(&p.Path, "path", "", "build from this subdirectory (monorepos)")
	c.Flags().BoolVar(&p.Private, "private", false, "no public hostname: LAN/Tailscale only")
	c.Flags().BoolVar(&p.NoWebhook, "no-webhook", false, "poll the branch every 60 s instead of registering a webhook")
	c.Flags().BoolVar(&p.NoDeploy, "no-deploy", false, "register only; deploy later")
	return c
}

// normalizeRepo accepts github URLs and turns them into owner/name.
func normalizeRepo(r string) string {
	for _, pre := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "github.com/"} {
		if strings.HasPrefix(r, pre) {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r, pre), "/"), ".git")
		}
	}
	if strings.HasPrefix(r, ".") || (strings.HasPrefix(r, "/") && !strings.HasPrefix(r, "//")) {
		if abs, err := absPath(r); err == nil {
			return abs
		}
	}
	return r
}

func cmdDeploy() *cobra.Command {
	var p daemon.AppParams
	c := &cobra.Command{
		Use:   "deploy <app> [--ref sha]",
		Short: "Build and release now (also applies config changes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			p.App = app
			if os.Getenv("GIT_DIR") != "" {
				p.Trigger = "push"
			}
			var res daemon.ReleaseResult
			if err := call("deploy", p, &res); err != nil {
				return hintErr(err, fmt.Sprintf("build log: berth logs %s --build", app))
			}
			if !jsonOut {
				printLive(res)
			}
			return nil
		},
	}
	c.Flags().StringVar(&p.Ref, "ref", "", "branch, tag or commit to deploy (default: the app's branch)")
	c.Flags().BoolVar(&p.NoCache, "no-cache", false, "build without the layer cache")
	return c
}

func printLive(res daemon.ReleaseResult) {
	u := ""
	if len(res.URLs) > 0 {
		u = " at " + res.URLs[0]
	}
	sha := res.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	fmt.Printf("=====> %s v%d (%s) is live%s\n", res.App, res.Release, sha, u)
}

func cmdRollback() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <app> [n]",
		Short: "Swap back to the previous release (or release n)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			p := daemon.AppParams{App: app}
			if len(args) == 2 {
				fmt.Sscan(strings.TrimPrefix(args[1], "v"), &p.N)
			}
			var res daemon.ReleaseResult
			if err := call("rollback", p, &res); err != nil {
				return err
			}
			if !jsonOut {
				printLive(res)
			}
			return nil
		},
	}
}

func simpleAppCmd(use, short, method string, after func(app string)) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <app>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			var res daemon.ReleaseResult
			if err := call(method, daemon.AppParams{App: app}, &res); err != nil {
				return err
			}
			if !jsonOut && after != nil {
				after(app)
			} else if !jsonOut && res.Release != 0 {
				printLive(res)
			}
			return nil
		},
	}
}

func cmdRestart() *cobra.Command {
	return simpleAppCmd("restart", "Start a fresh container for the current release (zero downtime)", "restart", nil)
}

func cmdStop() *cobra.Command {
	return simpleAppCmd("stop", "Stop an app (its URL answers 503)", "stop", func(string) {})
}

func cmdStart() *cobra.Command {
	return simpleAppCmd("start", "Start a stopped app", "start", nil)
}

func cmdReload() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Re-apply ~/.berth to containers, Caddy and the tunnel (after hand edits)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("reload", nil, nil)
		},
	}
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func cmdDestroy() *cobra.Command {
	var keepData, yes bool
	c := &cobra.Command{
		Use:   "destroy <app>",
		Short: "Remove an app, its hostname, webhook and images",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Destroy %s? This removes its containers, images, releases and config.", app)) {
				return fmt.Errorf("aborted")
			}
			return call("apps.destroy", daemon.AppParams{App: app, KeepData: keepData}, nil)
		},
	}
	c.Flags().BoolVar(&keepData, "keep-data", false, "keep the app's storage:mount folders")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return c
}

func cmdReleases() *cobra.Command {
	return &cobra.Command{
		Use:   "releases <app>",
		Short: "List releases with sha, status and time",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			s := store()
			rs, err := s.Releases(app)
			if err != nil {
				return err
			}
			cur := s.Current(app)
			if jsonOut {
				type row struct {
					*state.Release
					Current bool `json:"current"`
				}
				var rows []row
				for _, r := range rs {
					r.Env = nil
					rows = append(rows, row{r, r.N == cur})
				}
				printJSON(rows)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "  REL\tSHA\tSTATUS\tTRIGGER\tWHEN\tMESSAGE")
			for _, r := range rs {
				mark := " "
				if r.N == cur {
					mark = "*"
				}
				status := r.Status
				if r.Pruned {
					status += " (gc)"
				}
				msg := r.Message
				if r.Error != "" {
					msg = r.Error
				}
				if len(msg) > 60 {
					msg = msg[:57] + "..."
				}
				fmt.Fprintf(w, "%s v%d\t%s\t%s\t%s\t%s\t%s\n", mark, r.N, short(r.SHA), status, r.Trigger, ago(r.CreatedAt), msg)
			}
			return w.Flush()
		},
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// ---- domains ----

func cmdDomainsAdd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "domains:add <app> <host>",
		Short: "Serve an app on an extra hostname through the tunnel",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("domains.add", daemon.DomainParams{App: app, Host: args[1], Force: force}, nil)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "replace an existing DNS record for the hostname")
	return c
}

func cmdDomainsRemove() *cobra.Command {
	return &cobra.Command{
		Use:   "domains:remove <app> <host>",
		Short: "Remove an extra hostname",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("domains.remove", daemon.DomainParams{App: app, Host: args[1]}, nil)
		},
	}
}

func cmdDomainsList() *cobra.Command {
	return &cobra.Command{
		Use:   "domains:list <app>",
		Short: "List an app's hostnames",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			a, err := store().App(app)
			if err != nil {
				return err
			}
			cfg, err := store().Config()
			if err != nil {
				return err
			}
			urls := daemon.AppURLs(cfg, a)
			if jsonOut {
				printJSON(urls)
				return nil
			}
			for _, u := range urls {
				fmt.Println(u)
			}
			return nil
		},
	}
}

// ---- auth ----

func cmdAuthSet() *cobra.Command {
	return &cobra.Command{
		Use:   "auth:set <app> <user:password>",
		Short: "Require HTTP basic auth (enforced by Caddy)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			user, pass, ok := strings.Cut(args[1], ":")
			if !ok {
				return fmt.Errorf("use user:password")
			}
			return call("auth.set", daemon.AuthParams{App: app, User: user, Password: pass}, nil)
		},
	}
}

func cmdAuthUnset() *cobra.Command {
	return &cobra.Command{
		Use:   "auth:unset <app>",
		Short: "Remove basic auth",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("auth.unset", daemon.AuthParams{App: app}, nil)
		},
	}
}

// ---- storage ----

func cmdStorageMount() *cobra.Command {
	var noRestart bool
	c := &cobra.Command{
		Use:     "storage:mount <app> <container-path>",
		Short:   "Persist a folder across deploys (SQLite, uploads)",
		Example: "  berth storage:mount myapp /data   # then DATABASE_PATH=/data/app.db",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return configCall2("storage.mount", daemon.StorageParams{App: app, Path: args[1], NoRestart: noRestart})
		},
	}
	c.Flags().BoolVar(&noRestart, "no-restart", false, "apply on the next deploy")
	return c
}

func cmdStorageUnmount() *cobra.Command {
	var noRestart bool
	c := &cobra.Command{
		Use:   "storage:unmount <app> <container-path>",
		Short: "Stop mounting a folder (the data is kept)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return configCall2("storage.unmount", daemon.StorageParams{App: app, Path: args[1], NoRestart: noRestart})
		},
	}
	c.Flags().BoolVar(&noRestart, "no-restart", false, "apply on the next deploy")
	return c
}

func cmdStorageList() *cobra.Command {
	return &cobra.Command{
		Use:   "storage:list <app>",
		Short: "List persistent folders",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			a, err := store().App(app)
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(a.Mounts)
				return nil
			}
			for _, m := range a.Mounts {
				fmt.Printf("%s → %s\n", m.Source, m.Path)
			}
			return nil
		},
	}
}

func cmdSetKey() *cobra.Command {
	return &cobra.Command{
		Use:   "app:set-key <app>",
		Short: "Fetch over SSH with a generated deploy key instead of the GitHub token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("app.setkey", daemon.AppParams{App: app}, nil)
		},
	}
}

func cmdSetRepo() *cobra.Command {
	var branch string
	c := &cobra.Command{
		Use:     "app:set-repo <app> <owner/name|url|path>",
		Short:   "Deploy an app from another repository or branch (keeps config, data and releases)",
		Example: "  berth app:set-repo chirp dps/berth_chirp_node",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("app.setrepo", daemon.SetRepoParams{App: app, Repo: normalizeRepo(args[1]), Branch: branch}, nil)
		},
	}
	c.Flags().StringVar(&branch, "branch", "", "branch to deploy (default: keep the current one)")
	return c
}

func cmdDockerfile() *cobra.Command {
	var ref string
	c := &cobra.Command{
		Use:   "dockerfile <app>",
		Short: "Print the Dockerfile Berth would build with",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			var res map[string]string
			raw, err := client().Call(cmd.Context(), "dockerfile", daemon.AppParams{App: app, Ref: ref}, os.Stderr, &res)
			if err != nil {
				return err
			}
			if jsonOut {
				fmt.Println(string(raw))
				return nil
			}
			if res["stack"] == "dockerfile" {
				fmt.Fprintln(os.Stderr, "# (the repo's own Dockerfile)")
			}
			fmt.Print(res["dockerfile"])
			return nil
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "branch or commit (default: the app's branch)")
	return c
}
