package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/doctor"
	"github.com/dps/berth/internal/launchd"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/tunnel"
)

func cmdDoctor() *cobra.Command {
	var images bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check every assumption Berth makes and print a fix for each failure",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			d := &doctor.Doctor{Store: store(), RT: runtime.NewApple()}
			checks := d.Run(ctx, images)
			if jsonOut {
				printJSON(checks)
			} else {
				printChecks(checks)
			}
			for _, c := range checks {
				if c.Level == doctor.Fail {
					return fmt.Errorf("some checks failed")
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&images, "images", false, "also list base images with newer versions available")
	return c
}

func printChecks(checks []doctor.Check) {
	for _, c := range checks {
		mark := map[doctor.Level]string{doctor.OK: "✓", doctor.Warn: "!", doctor.Fail: "✗", doctor.Skip: "-"}[c.Level]
		line := fmt.Sprintf(" %s %s", mark, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Println(line)
		if c.Fix != "" && c.Level != doctor.OK {
			fmt.Printf("     fix: %s\n", c.Fix)
		}
	}
}

func cmdGC() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Prune old releases' images, old dumps and leftover build dirs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("gc", nil, nil)
		},
	}
}

func cmdBackup() *cobra.Command {
	return &cobra.Command{
		Use:   "backup [archive.tar.gz]",
		Short: "Archive ~/.berth (state, env, fresh dumps, app volumes) into one file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := ""
			if len(args) == 1 {
				abs, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				p = abs
			}
			return call("backup", map[string]string{"path": p}, nil)
		},
	}
}

func cmdRestore() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <archive.tar.gz>",
		Short: "Recreate apps, services and links from a backup (after `berth init` on a fresh Mac)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			return call("restore", map[string]string{"archive": abs}, nil)
		},
	}
}

func cmdNotifySet() *cobra.Command {
	return &cobra.Command{
		Use:   "notify:set <url|off>",
		Short: "Send deploy and crash-loop events to a webhook (Slack, Discord, ntfy)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u := args[0]
			if u == "off" {
				u = ""
			}
			return call("notify.set", map[string]string{"url": u}, nil)
		},
	}
}

func cmdOnVisitSet() *cobra.Command {
	var apps []string
	c := &cobra.Command{
		Use:   "on-visit:set <command|off>",
		Short: "Run a command on the Mac when someone opens a page (flash a blink(1), ring a bell)",
		Long: `Run a shell command on the Mac each time someone opens a page of your
apps: a GET from a browser for a page, not its images or scripts, and not
bots. It runs one at a time; page views that arrive meanwhile are counted
in BERTH_VISITS for the next run. BERTH_APP, BERTH_HOST, BERTH_PATH,
BERTH_COUNTRY, BERTH_REFERRER and BERTH_MOBILE describe the latest one.

  berth on-visit:set 'blink1-tool -q --rgb=0,90,255 --blink 1'
  berth on-visit:set 'blink1-tool -q --blink 1' --app site
  berth on-visit:set off`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			command := args[0]
			if command == "off" {
				command = ""
			}
			return call("onvisit.set", map[string]any{"command": command, "apps": apps}, nil)
		},
	}
	c.Flags().StringSliceVar(&apps, "app", nil, "only page views of this app (repeat or comma-separate for more)")
	return c
}

func cmdReboot() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "reboot",
		Short: "Planned reboot that comes back unattended (fdesetup authrestart)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fv := daemon.FileVaultStatus()
			var argv []string
			if fv == "on" {
				argv = []string{"sudo", "fdesetup", "authrestart"}
				fmt.Println("FileVault is on: `fdesetup authrestart` unlocks the disk for the next boot only,")
				fmt.Println("so this Mac logs back in and Berth starts without anyone at the screen.")
			} else {
				argv = []string{"sudo", "shutdown", "-r", "now"}
			}
			if !yes && !confirm("Reboot now with `"+strings.Join(argv, " ")+"`?") {
				return fmt.Errorf("aborted")
			}
			c := exec.Command(argv[0], argv[1:]...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return c
}

func caRootPath() string { return store().Path("caddy", "pki", "authorities", "local", "root.crt") }

func trustCA() error {
	root := caRootPath()
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("Caddy has not created its local CA yet (it does on the first LAN route); try again after `berth create`")
	}
	home, _ := os.UserHomeDir()
	c := exec.Command("security", "add-trusted-cert", "-r", "trustRoot", "-k", filepath.Join(home, "Library", "Keychains", "login.keychain-db"), root)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func cmdCFLogin() *cobra.Command {
	return &cobra.Command{
		Use:     "cf:login <zone>",
		Short:   "Connect another Cloudflare zone by logging in through the browser (for domains:add)",
		Example: "  berth cf:login mysite.com\n  berth domains:add myapp www.mysite.com",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			zone := strings.ToLower(strings.TrimSuffix(args[0], "."))
			s := store()
			if _, err := s.Config(); err != nil {
				return err
			}
			if _, err := cfLogin(cmd.Context(), s, &tunnel.CLI{Bin: runtime.LookBin("cloudflared")}, zone); err != nil {
				return err
			}
			fmt.Printf("-----> Connected %s; add hostnames with `berth domains:add <app> <host>.%s`\n", zone, zone)
			return nil
		},
	}
}

func cmdTrustCA() *cobra.Command {
	return &cobra.Command{
		Use:   "trust-ca",
		Short: "Trust Caddy's local CA in the login keychain (lock icon for *.local URLs)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := trustCA(); err != nil {
				return err
			}
			fmt.Println("-----> Trusted " + caRootPath())
			fmt.Println("       For other devices, install that root.crt as a trusted profile.")
			return nil
		},
	}
}

func cmdUninstall() *cobra.Command {
	var purge, yes bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop everything and remove the launchd agents (--purge also deletes data)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			what := "stop all apps and services and remove Berth's launchd agents (data in ~/.berth is kept)"
			if purge {
				what = "DESTROY every app, webhook, database volume, image and ~/.berth itself"
			}
			if !yes && !confirm("This will "+what+". Continue?") {
				return fmt.Errorf("aborted")
			}
			ctx := context.Background()
			if purge {
				if apps, _ := store().Apps(); len(apps) > 0 {
					for _, a := range apps {
						if err := call("apps.destroy", daemon.AppParams{App: a.Name}, nil); err != nil {
							fmt.Fprintf(os.Stderr, " !     %s: %v\n", a.Name, err)
						}
					}
				}
			}
			for _, l := range []string{launchd.LabelBerthd, launchd.LabelCloudflared, launchd.LabelCaddy} {
				if err := launchd.Uninstall(l); err != nil {
					fmt.Fprintf(os.Stderr, " !     %s: %v\n", l, err)
				}
			}
			fmt.Println("-----> Removed launchd agents")
			rt := runtime.NewApple()
			if cs, err := rt.List(ctx); err == nil {
				for _, c := range cs {
					if c.Labels["dev.berth.app"] != "" || c.Labels["dev.berth.service"] != "" {
						rt.Remove(ctx, c.ID)
					}
				}
			}
			fmt.Println("-----> Stopped Berth containers")
			if purge {
				// Every berth-* volume, including ones pg:upgrade kept.
				if vols, err := rt.Volumes(ctx); err == nil {
					for _, v := range vols {
						if strings.HasPrefix(v, "berth-") {
							rt.VolumeRemove(ctx, v)
						}
					}
				}
				if imgs, err := rt.Images(ctx); err == nil {
					var ours []string
					for _, i := range imgs {
						if strings.HasPrefix(i.Name, "berth/") {
							ours = append(ours, i.Name)
						}
					}
					rt.RemoveImage(ctx, ours...)
				}
				os.RemoveAll(store().Root)
				fmt.Println("-----> Deleted volumes, images and " + store().Root)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete apps, webhooks, volumes, images and ~/.berth")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return c
}
