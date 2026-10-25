package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/services"
	"github.com/dps/berth/internal/state"
)

// serviceCommands builds the pg:* or redis:* family (same verbs as
// dokku-postgres / dokku-redis).
func serviceCommands(kind string) []*cobra.Command {
	k := services.Kinds[kind]
	label := map[string]string{"pg": "Postgres", "redis": "Redis"}[kind]
	svc := func(args []string) (*state.Service, error) {
		sv, err := store().Service(kind, args[0])
		if err != nil {
			return nil, hintErr(err, fmt.Sprintf("`berth %s:list` lists them", kind))
		}
		return sv, nil
	}

	var image, memory string
	create := &cobra.Command{
		Use:   kind + ":create <name>",
		Short: fmt.Sprintf("Create a %s service (%s)", label, k.Image),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("svc.create", daemon.SvcParams{Kind: kind, Name: args[0], Image: image, Memory: memory}, nil)
		},
	}
	create.Flags().StringVar(&image, "image", "", "image (default "+k.Image+")")
	create.Flags().StringVar(&memory, "memory", "", "memory limit (default "+k.Memory+")")

	var as string
	var noRestart bool
	link := &cobra.Command{
		Use:   kind + ":link <name> <app>",
		Short: fmt.Sprintf("Give an app its own credentials and set %s, then redeploy", k.EnvVar),
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := svc(args); err != nil {
				return err
			}
			if _, err := appArg(args[1:]); err != nil {
				return err
			}
			return configCall2("svc.link", daemon.SvcParams{Kind: kind, Name: args[0], App: args[1], As: as, NoRestart: noRestart})
		},
	}
	link.Flags().StringVar(&as, "as", "", "env var name (default "+k.EnvVar+")")
	link.Flags().BoolVar(&noRestart, "no-restart", false, "do not redeploy")

	unlink := &cobra.Command{
		Use:   kind + ":unlink <name> <app>",
		Short: "Revoke an app's access and remove its env var",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return configCall2("svc.unlink", daemon.SvcParams{Kind: kind, Name: args[0], App: args[1]})
		},
	}

	clientName := map[string]string{"pg": "psql", "redis": "cli"}[kind]
	shell := &cobra.Command{
		Use:   kind + ":" + clientName + " <name> [args...]",
		Short: "Open an interactive " + map[string]string{"pg": "psql", "redis": "redis-cli"}[kind] + " session",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sv, err := svc(args)
			if err != nil {
				return err
			}
			var inner []string
			if kind == "pg" {
				inner = append([]string{"psql", "-U", "postgres", strings.ReplaceAll(sv.Name, "-", "_")}, args[1:]...)
			} else {
				inner = []string{"sh", "-c", `exec redis-cli -a "$REDIS_PASSWORD" --no-auth-warning "$@"`, "redis-cli"}
				inner = append(inner, args[1:]...)
			}
			return execContainer(state.ServiceContainer(kind, sv.Name), inner)
		},
	}

	shell.Flags().SetInterspersed(false) // pass psql/redis-cli flags through

	backup := &cobra.Command{
		Use:   kind + ":backup <name>",
		Short: "Dump to ~/.berth/backups/" + kind + "/<name>/",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("svc.backup", daemon.SvcParams{Kind: kind, Name: args[0]}, nil)
		},
	}

	restore := &cobra.Command{
		Use:   kind + ":restore <name> <file>",
		Short: "Load a dump (replaces current data)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			return call("svc.restore", daemon.SvcParams{Kind: kind, Name: args[0], File: f}, nil)
		},
	}

	expose := &cobra.Command{
		Use:   kind + ":expose <name> <port|off>",
		Short: "Publish on 127.0.0.1:<port> for desktop clients",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port := 0
			if args[1] != "off" {
				p, err := strconv.Atoi(args[1])
				if err != nil || p < 1 || p > 65535 {
					return fmt.Errorf("port must be 1-65535 or off")
				}
				port = p
			}
			return call("svc.expose", daemon.SvcParams{Kind: kind, Name: args[0], Port: port}, nil)
		},
	}

	var force, keepData, yes bool
	destroy := &cobra.Command{
		Use:   kind + ":destroy <name>",
		Short: "Delete the service and its data volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := svc(args); err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Destroy %s:%s and its data? (backups in ~/.berth/backups are kept)", kind, args[0])) {
				return fmt.Errorf("aborted")
			}
			return call("svc.destroy", daemon.SvcParams{Kind: kind, Name: args[0], Force: force, KeepData: keepData}, nil)
		},
	}
	destroy.Flags().BoolVar(&force, "force", false, "destroy even if apps are linked")
	destroy.Flags().BoolVar(&keepData, "keep-data", false, "keep the data volume")
	destroy.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	list := &cobra.Command{
		Use:   kind + ":list",
		Short: "List " + label + " services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := store().Config()
			if err != nil {
				return err
			}
			svs, err := store().Services(kind)
			if err != nil {
				return err
			}
			if jsonOut {
				for _, s := range svs {
					s.Password = ""
					for a, l := range s.Links {
						l.Password = ""
						s.Links[a] = l
					}
				}
				printJSON(svs)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tIMAGE\tENDPOINT\tVOLUME\tLINKED TO")
			for _, s := range svs {
				var apps []string
				for a := range s.Links {
					apps = append(apps, a)
				}
				fmt.Fprintf(w, "%s\t%s\t%s:%d\t%s\t%s\n", s.Name, s.Image, cfg.Gateway, s.Port, s.Volume, strings.Join(apps, ","))
			}
			return w.Flush()
		},
	}

	cmds := []*cobra.Command{create, link, unlink, shell, backup, restore, expose, destroy, list}
	if kind == "pg" {
		cmds = append(cmds, &cobra.Command{
			Use:   "pg:upgrade <name> --to <major>",
			Short: "Move to a new Postgres major version (dump, new container, restore, re-link)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				to, _ := cmd.Flags().GetString("to")
				if to == "" {
					return fmt.Errorf("--to is required, e.g. --to 18")
				}
				return call("svc.upgrade", daemon.SvcParams{Kind: kind, Name: args[0], Version: to}, nil)
			},
		})
		cmds[len(cmds)-1].Flags().String("to", "", "target major version")
	}
	return cmds
}

// configCall2 is a config-changing call that may print a new release.
func configCall2(method string, p any) error {
	var res daemon.ReleaseResult
	if err := call(method, p, &res); err != nil {
		return err
	}
	if !jsonOut && res.Release != 0 {
		printLive(res)
	}
	return nil
}

// execContainer replaces the CLI with `container exec -it`, so signals and
// the terminal pass straight through.
func execContainer(name string, args []string) error {
	bin := runtime.LookBin("container")
	argv := []string{bin, "exec", "-i"}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		argv = append(argv, "-t")
	}
	argv = append(argv, name)
	argv = append(argv, args...)
	return syscall.Exec(bin, argv, os.Environ())
}
