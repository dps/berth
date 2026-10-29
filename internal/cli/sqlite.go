package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/state"
)

// sqliteCommands is the sqlite:* family: an app's own SQLite database
// (design.md, SQLite).
func sqliteCommands() []*cobra.Command {
	var as string
	var noRestart bool
	add := &cobra.Command{
		Use:   "sqlite:add <app>",
		Short: "Give an app its own SQLite database (" + state.SQLitePath + ")",
		Long: "Give an app its own SQLite database, on a volume only its one container can open.\n" +
			"Sets DATABASE_URL=" + state.SQLiteURL + " (or --as) and " + state.SQLitePathVar + "=" + state.SQLitePath + ".\n" +
			"From then on deploys stop the old release before starting the new one (a few seconds\n" +
			"of waiting for visitors), and cron jobs run inside the app, as workers do.\n" +
			"Backed up nightly with Postgres.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return configCall2("sqlite.add", daemon.SQLiteParams{App: app, As: as, NoRestart: noRestart})
		},
	}
	add.Flags().StringVar(&as, "as", "", "env var for the URL (default DATABASE_URL)")
	add.Flags().BoolVar(&noRestart, "no-restart", false, "apply on the next deploy")

	var deleteData, yes bool
	remove := &cobra.Command{
		Use:   "sqlite:remove <app>",
		Short: "Stop giving an app its SQLite database (kept unless --delete-data)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			if deleteData && !yes && !confirm(fmt.Sprintf("Delete %s's SQLite database? (backups in ~/.berth/backups/sqlite are kept)", app)) {
				return fmt.Errorf("aborted")
			}
			return configCall2("sqlite.remove", daemon.SQLiteParams{App: app, DeleteData: deleteData, NoRestart: noRestart})
		},
	}
	remove.Flags().BoolVar(&deleteData, "delete-data", false, "also delete the database's volume")
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	remove.Flags().BoolVar(&noRestart, "no-restart", false, "apply on the next deploy")

	var stop bool
	backup := &cobra.Command{
		Use:   "sqlite:backup <app>",
		Short: "Copy the database to ~/.berth/backups/sqlite/<app>/ (while it runs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			return call("sqlite.backup", daemon.SQLiteParams{App: app, Stop: stop}, nil)
		},
	}
	backup.Flags().BoolVar(&stop, "stop", false, "if the image has no sqlite3 or Python, stop the app for a few seconds to copy it")

	restore := &cobra.Command{
		Use:   "sqlite:restore <app> <file>",
		Short: "Replace the database with a backup (stops the app for a few seconds)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			f, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("Replace %s's database with %s? (the current one is saved first)", app, filepath.Base(f))) {
				return fmt.Errorf("aborted")
			}
			return call("sqlite.restore", daemon.SQLiteParams{App: app, File: f}, nil)
		},
	}
	restore.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	shell := &cobra.Command{
		Use:   "sqlite:shell <app> [sql]",
		Short: "Open the database inside the app's container (sqlite3, or Python's)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			a, err := store().App(app)
			if err != nil {
				return err
			}
			if a.SQLite == nil {
				return fmt.Errorf("%s has no SQLite database (berth sqlite:add %s)", app, app)
			}
			rel := store().CurrentRelease(app)
			if rel == nil || rel.Container == "" {
				return fmt.Errorf("%s isn't running; the database is only opened inside its container (berth start %s)", app, app)
			}
			// python -m sqlite3 is Python 3.12's shell.
			script := `db=` + state.SQLitePath + `
if command -v sqlite3 >/dev/null 2>&1; then exec sqlite3 "$db" "$@"; fi
py=$(command -v python3 || command -v python) && "$py" -c 'import sqlite3.__main__' 2>/dev/null && exec "$py" -m sqlite3 "$db" "$@"
echo "The image has no sqlite3 (or Python 3.12+). Add sqlite3 to it, or use: berth sqlite:backup ` + app + ` and open the copy on the Mac." >&2
exit 1`
			return execContainer(rel.Container, append([]string{"sh", "-c", script, "sqlite-shell"}, args[1:]...))
		},
	}

	list := &cobra.Command{
		Use:   "sqlite:list",
		Short: "List apps with a SQLite database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var list []daemon.SQLiteInfo
			if err := call("sqlite.list", nil, &list); err != nil {
				return err
			}
			if jsonOut {
				printJSON(list)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tENV\tVOLUME\tLAST BACKUP")
			for _, s := range list {
				last := "–"
				if s.LastBackup != "" {
					last = filepath.Base(s.LastBackup)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.App, s.EnvVar, s.Volume, last)
			}
			return w.Flush()
		},
	}
	return []*cobra.Command{add, remove, backup, restore, shell, list}
}
