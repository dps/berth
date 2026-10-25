package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

func cmdRun() *cobra.Command {
	c := &cobra.Command{
		Use:   "run <app> <command...>",
		Short: "Run a one-off command in a fresh container with the app's env (migrations)",
		Example: "  berth run myapp npx prisma migrate deploy\n" +
			"  berth run myapp python manage.py migrate",
		Args:               cobra.MinimumNArgs(2),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			s := store()
			rel := s.CurrentRelease(app)
			if rel == nil || rel.Pruned {
				return fmt.Errorf("%s has no deployed image yet; deploy it first", app)
			}
			cfg, err := s.Config()
			if err != nil {
				return err
			}
			a, err := s.App(app)
			if err != nil {
				return err
			}
			env, err := s.Env(app)
			if err != nil {
				return err
			}
			// Live config (not the release snapshot): migrations want the
			// settings you just changed.
			file, extra := state.Env(deploy.ContainerEnv(env)).ContainerEnvFile()
			tmp, err := os.CreateTemp(s.Path("apps", app), ".run-env-*")
			if err != nil {
				return err
			}
			tmp.Chmod(0o600)
			tmp.WriteString(file)
			tmp.Close()
			defer os.Remove(tmp.Name())
			var vols []string
			for _, m := range a.Mounts {
				vols = append(vols, m.Source+":"+m.Path)
			}
			tty := term.IsTerminal(int(os.Stdin.Fd()))
			ap := runtime.NewApple()
			argv := ap.RunArgs(runtime.RunOpts{
				Image:       rel.Image,
				Network:     cfg.Network,
				EnvFile:     tmp.Name(),
				Env:         extra,
				Memory:      env.StrSetting(state.KeyMemory, cfg.DefaultMemory),
				CPUs:        env.StrSetting(state.KeyCPUs, cfg.DefaultCPUs),
				Volumes:     vols,
				Labels:      map[string]string{"dev.berth.run": app},
				Init:        true,
				Remove:      true,
				Interactive: true,
				TTY:         tty,
				Entrypoint:  args[1],
				Args:        args[2:],
			})
			c := exec.Command(ap.Bin, argv...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					os.Remove(tmp.Name())
					os.Exit(ee.ExitCode())
				}
				return err
			}
			return nil
		},
	}
	c.Flags().SetInterspersed(false)
	return c
}

func cmdExec() *cobra.Command {
	c := &cobra.Command{
		Use:   "exec <app> [command...]",
		Short: "Run a command (default: a shell) inside the running container",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			rel := store().CurrentRelease(app)
			if rel == nil || rel.Container == "" {
				return fmt.Errorf("%s is not running", app)
			}
			inner := args[1:]
			if len(inner) == 0 {
				inner = []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}
			}
			return execContainer(rel.Container, inner)
		},
	}
	c.Flags().SetInterspersed(false)
	return c
}

func cmdGitRemote() *cobra.Command {
	var host, name string
	c := &cobra.Command{
		Use:   "git:remote <app>",
		Short: "Add a `berth` git remote so `git push berth main` deploys over SSH",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			var res map[string]string
			if err := call("git.hook", daemon.AppParams{App: app}, &res); err != nil {
				return err
			}
			cfg, err := store().Config()
			if err != nil {
				return err
			}
			if host == "" {
				host = cfg.Hostname + ".local"
			}
			u, _ := user.Current()
			url := fmt.Sprintf("ssh://%s@%s%s", u.Username, host, res["repo"])
			if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
				fmt.Printf("Not in a git repo. In your app's repo run:\n  git remote add %s %s\n", name, url)
				return nil
			}
			exec.Command("git", "remote", "remove", name).Run()
			if out, err := exec.Command("git", "remote", "add", name, url).CombinedOutput(); err != nil {
				return fmt.Errorf("git remote add: %s", out)
			}
			fmt.Printf("-----> Added remote %q → %s\n", name, url)
			fmt.Printf("       Deploy with: git push %s %s\n", name, res["branch"])
			fmt.Println("       Needs SSH to this Mac: System Settings → General → Sharing → Remote Login")
			return nil
		},
	}
	c.Flags().StringVar(&host, "host", "", "SSH host of the Mac (default <hostname>.local; use a Tailscale name for remote)")
	c.Flags().StringVar(&name, "name", "berth", "remote name")
	return c
}
