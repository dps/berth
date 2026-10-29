package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

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
			// A SQLite database is open in one container at a time: run the
			// command in the app's, with that release's config.
			if a.SQLite != nil && rel.Container != "" {
				fmt.Fprintf(os.Stderr, "-----> Running inside %s (it has SQLite, so not in a new container)\n", rel.Container)
				return execContainer(rel.Container, args[1:])
			}
			env, err := s.Env(app)
			if err != nil {
				return err
			}
			// Env files left by a `berth run` that was killed (SIGKILL skips
			// cleanup) hold live secrets; sweep them.
			if old, _ := filepath.Glob(s.Path("apps", app, ".run-env-*")); len(old) > 0 {
				for _, f := range old {
					if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > time.Hour {
						os.Remove(f)
					}
				}
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
			vols := a.Volumes()
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
	var printURL bool
	c := &cobra.Command{
		Use:   "git:remote <app>",
		Short: "Add a `berth` git remote so `git push berth main` deploys over SSH",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isBerthMac() {
				t, err := berthTarget()
				if err != nil {
					return err
				}
				return addRemote(t, args[0], host, name)
			}
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
			if printURL {
				fmt.Println(url)
				return nil
			}
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
	c.Flags().StringVar(&host, "host", "", "SSH host of the Mac (default <hostname>.local)")
	c.Flags().StringVar(&name, "name", "berth", "remote name")
	c.Flags().BoolVar(&printURL, "print", false, "only print the remote's URL")
	c.Flags().MarkHidden("print")
	return c
}

// addRemote adds the `berth` remote on a machine without Berth: the Mac
// works out the URL, with the host as this machine reaches it.
func addRemote(t *sshTarget, app, host, name string) error {
	if host == "" {
		host = t.hostname()
		if t.port != "" {
			host += ":" + t.port
		}
	}
	out, err := t.output("git:remote", app, "--host", host, "--print")
	if err != nil {
		return err
	}
	url := strings.TrimSpace(string(out))
	if _, err := gitIn(".", nil, "rev-parse", "--is-inside-work-tree"); err != nil {
		fmt.Printf("Not in a git repo. In your app's repo run:\n  git remote add %s %s\n", name, url)
		return nil
	}
	exec.Command("git", "remote", "remove", name).Run()
	if out, err := exec.Command("git", "remote", "add", name, url).CombinedOutput(); err != nil {
		return fmt.Errorf("git remote add: %s", out)
	}
	fmt.Printf("-----> Added remote %q → %s\n", name, url)
	fmt.Println("       berth commands in this repo now reach the Mac, and `berth dev:push` deploys from here")
	return nil
}
