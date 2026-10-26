package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/state"
)

func cmdStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status [app]",
		Short: "Every app, release, uptime, URL and disk at a glance",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var st daemon.Status
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			offline := false
			if _, err := client().Call(ctx, "status", nil, nil, &st); err != nil {
				s, cerr := offlineStatus()
				if cerr != nil {
					return cerr
				}
				st, offline = *s, true
				fmt.Fprintf(os.Stderr, " !     berthd is not answering (%v); showing saved state\n", err)
			}
			if len(args) == 1 {
				if _, err := appArg(args); err != nil {
					return err
				}
				var apps []daemon.AppStatus
				for _, a := range st.Apps {
					if a.Name == args[0] {
						apps = append(apps, a)
					}
				}
				st.Apps = apps
				st.Services = nil
			}
			if jsonOut {
				printJSON(st)
				return nil
			}
			printStatus(&st, offline, len(args) == 1)
			return nil
		},
	}
}

// offlineStatus builds what it can from state files alone.
func offlineStatus() (*daemon.Status, error) {
	s := store()
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	st := &daemon.Status{}
	st.System.Domain = cfg.Domain
	free, _ := deploy.FreeBytes(s.Root)
	st.System.DiskFreeBytes = free
	apps, _ := s.Apps()
	for _, a := range apps {
		as := daemon.AppStatus{Name: a.Name, State: "unknown", URLs: daemon.AppURLs(cfg, a), Repo: a.Repo, Branch: a.Branch}
		if r := s.CurrentRelease(a.Name); r != nil {
			as.Release, as.SHA, as.HostPort, as.Message = r.N, r.SHA, r.HostPort, r.Message
		}
		st.Apps = append(st.Apps, as)
	}
	svs, _ := s.Services("")
	for _, sv := range svs {
		st.Services = append(st.Services, daemon.ServiceStatus{Kind: sv.Kind, Name: sv.Name, Image: sv.Image, Volume: sv.Volume})
	}
	return st, nil
}

func yesno(ok bool, yes, no string) string {
	if ok {
		return "✓ " + yes
	}
	return "✗ " + no
}

func printStatus(st *daemon.Status, offline, oneApp bool) {
	sys := st.System
	if !oneApp {
		if !offline {
			tun := "off"
			if sys.TunnelMode != "off" {
				tun = yesno(sys.TunnelConnected, "connected", "down") + " (" + sys.TunnelMode + ")"
			}
			domain := sys.Domain
			if domain == "" {
				domain = "LAN only"
			}
			fmt.Printf("berth %s · %s · berthd up %s\n", sys.Version, domain, sys.Uptime)
			fmt.Printf("container %s · caddy %s · tunnel %s · disk %s free",
				yesno(sys.ContainerRunning, "running", "down"), yesno(sys.CaddyUp, "up", "down"), tun, deploy.HumanBytes(int64(sys.DiskFreeBytes)))
			if sys.MemPhysical > 0 {
				fmt.Printf(" · memory %s of %s committed", deploy.HumanBytes(sys.MemCommitted), deploy.HumanBytes(sys.MemPhysical))
			}
			fmt.Println()
			if sys.AI != "" {
				loaded := "no model in memory"
				if len(sys.AILoaded) > 0 {
					loaded = strings.Join(sys.AILoaded, ", ") + " in memory (" + deploy.HumanBytes(sys.AIMem) + ")"
				}
				fmt.Printf("ai %s · %s\n", yesno(sys.AI == "up", "up", "down: Ollama not answering"), loaded)
			}
		}
		fmt.Println()
	}
	if len(st.Apps) == 0 {
		fmt.Println("No apps yet: berth create <app> --repo owner/name")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "APP\tSTATE\tREL\tSHA\tUPTIME\tMEM\tPORT\tURL\tLAST DEPLOY")
		for _, a := range st.Apps {
			up := "-"
			if !a.Since.IsZero() {
				up = dur(time.Since(a.Since))
			}
			mem := "-"
			if a.MemoryBytes > 0 {
				mem = fmt.Sprintf("%s/%s", deploy.HumanBytes(a.MemoryBytes), a.MemoryLimit)
			}
			url := "-"
			if len(a.URLs) > 0 {
				url = a.URLs[0]
			}
			last := "-"
			if a.LastDeploy != nil {
				last = fmt.Sprintf("v%d %s %s", a.LastDeploy.Release, a.LastDeploy.Status, ago(a.LastDeploy.Time))
			}
			rel, port := "-", "-"
			if a.Release > 0 {
				rel = fmt.Sprintf("v%d", a.Release)
			}
			if a.HostPort > 0 {
				port = fmt.Sprint(a.HostPort)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.State, rel, short(a.SHA), up, mem, port, url, last)
		}
		w.Flush()
	}
	if oneApp && len(st.Apps) == 1 {
		a := st.Apps[0]
		fmt.Println()
		if a.Repo != "" {
			fmt.Printf("repo:   %s@%s\n", a.Repo, a.Branch)
		}
		for _, u := range a.URLs {
			fmt.Printf("url:    %s\n", u)
		}
		if a.Message != "" {
			fmt.Printf("commit: %s\n", a.Message)
		}
		if a.LastDeploy != nil && a.LastDeploy.Error != "" {
			fmt.Printf("last deploy error: %s\n", a.LastDeploy.Error)
		}
		for _, l := range a.Links {
			fmt.Printf("link:   %s\n", l)
		}
		for _, w := range a.Workers {
			st := "not running"
			if w.Running {
				st = "running, started " + ago(w.Since)
			}
			fmt.Printf("worker: %s %s (%s)\n", w.Name, st, w.Command)
		}
		if a.CronJobs > 0 {
			fmt.Printf("cron:   %d job(s); berth cron %s lists them\n", a.CronJobs, a.Name)
		}
	}
	if len(st.Services) > 0 {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tIMAGE\tSTATE\tMEM\tENDPOINT\tLINKED TO\tLAST BACKUP")
		for _, s := range st.Services {
			state := "stopped"
			if s.Running {
				state = "running"
			}
			if offline {
				state = "unknown"
			}
			mem := "-"
			if s.MemoryBytes > 0 {
				mem = deploy.HumanBytes(s.MemoryBytes)
			}
			links := "-"
			if len(s.Links) > 0 {
				links = fmt.Sprint(s.Links)
			}
			backup := "-"
			if s.LastBackup != "" {
				if fi, err := os.Stat(s.LastBackup); err == nil {
					backup = ago(fi.ModTime())
				}
			}
			fmt.Fprintf(w, "%s:%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Kind, s.Name, s.Image, state, mem, s.Endpoint, links, backup)
		}
		w.Flush()
	}
	if !oneApp && len(sys.Warnings) > 0 {
		fmt.Println()
		for _, warn := range sys.Warnings {
			fmt.Printf(" !  %s\n", warn)
		}
	}
}

func cmdEvents() *cobra.Command {
	var follow bool
	var n int
	c := &cobra.Command{
		Use:   "events",
		Short: "Show berthd's event log (deploys, restarts, webhooks)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := store().EventsPath()
			show := func(line string) {
				if jsonOut {
					fmt.Println(line)
					return
				}
				var ev state.Event
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					return
				}
				app := ev.App
				if app == "" {
					app = "-"
				}
				fmt.Printf("%s  %-16s %-12s %s\n", ev.Time.Local().Format("2006-01-02 15:04:05"), ev.Kind, app, ev.Message)
			}
			lines, _ := tailLines(path, n)
			for _, l := range lines {
				show(l)
			}
			if !follow {
				return nil
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			followFile(ctx, path, show)
			return nil
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events")
	c.Flags().IntVarP(&n, "lines", "n", 50, "number of events")
	return c
}
