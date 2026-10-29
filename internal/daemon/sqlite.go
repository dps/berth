package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/services"
	"github.com/dps/berth/internal/state"
)

// ---- SQLite (design.md, SQLite) ----

type SQLiteParams struct {
	App        string `json:"app"`
	As         string `json:"as,omitempty"`
	File       string `json:"file,omitempty"`
	Stop       bool   `json:"stop,omitempty"`
	DeleteData bool   `json:"delete_data,omitempty"`
	NoRestart  bool   `json:"no_restart,omitempty"`
}

// errNoSQLiteFile means the app hasn't created its database yet.
var errNoSQLiteFile = errors.New("no database yet (nothing at " + state.SQLitePath + ")")

// errNeedsStop means the app's image has no sqlite3 or Python to take a
// backup while it runs.
var errNeedsStop = errors.New("the app's image has neither sqlite3 nor Python, so a backup needs it stopped for a few seconds")

func (d *Daemon) hSQLiteAdd(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SQLiteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if app.DevOf != "" {
		return nil, fmt.Errorf("%s is a dev version or preview; add SQLite to %s and its copies get their own database", app.Name, app.DevOf)
	}
	if app.SQLite != nil {
		return nil, fmt.Errorf("%s already has a SQLite database (%s)", app.Name, app.SQLite.EnvVar)
	}
	envVar := p.As
	if envVar == "" {
		envVar = "DATABASE_URL"
	}
	if !envNameRe.MatchString(envVar) {
		return nil, fmt.Errorf("%q isn't a valid variable name", envVar)
	}
	env, err := d.Store.Env(app.Name)
	if err != nil {
		return nil, err
	}
	if v, ok := env[envVar]; ok && v != state.SQLiteURL {
		return nil, fmt.Errorf("%s already sets %s (a linked database?); pick another name with --as, like --as SQLITE_URL", app.Name, envVar)
	}
	if err := d.Deployer.SQLitePrepare(ctx, state.SQLiteVolume(app.Name)); err != nil {
		return nil, fmt.Errorf("creating the volume: %w", err)
	}
	children := []*state.App{app}
	if dev, err := d.Store.App(state.DevName(app.Name)); err == nil && dev.DevOf == app.Name {
		children = append(children, dev)
	}
	children = append(children, d.previewsOf(app.Name)...)
	for _, a := range children {
		if a != app {
			if err := d.Deployer.SQLitePrepare(ctx, state.SQLiteVolume(a.Name)); err != nil {
				return nil, fmt.Errorf("creating %s's volume: %w", a.Name, err)
			}
		}
		sq := &state.SQLite{Volume: state.SQLiteVolume(a.Name), EnvVar: envVar, CreatedAt: time.Now()}
		if err := d.updateApp(a.Name, func(a *state.App) { a.SQLite = sq }); err != nil {
			return nil, err
		}
		unlock := d.Deployer.Lock(a.Name)
		env, _ := d.Store.Env(a.Name)
		env[envVar], env[state.SQLitePathVar] = state.SQLiteURL, state.SQLitePath
		err := d.Store.SaveEnv(a.Name, env)
		unlock()
		if err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(out, "-----> %s has a SQLite database: %s=%s and %s=%s\n", app.Name, envVar, state.SQLiteURL, state.SQLitePathVar, state.SQLitePath)
	fmt.Fprintf(out, "       It lives on its own volume, open in one container at a time: deploys stop the\n")
	fmt.Fprintf(out, "       old release before starting the new one. Workers and cron jobs run inside the\n")
	fmt.Fprintf(out, "       app's container, so they can use it too.\n")
	if len(children) > 1 {
		fmt.Fprintf(out, "       Its dev version and previews get their own, empty, from their next push.\n")
	}
	d.event(state.Event{Kind: "sqlite.add", App: app.Name, Message: "SQLite database added (" + envVar + ")"})
	return d.redeployForConfig(ctx, app.Name, p.NoRestart, false, out)
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (d *Daemon) hSQLiteRemove(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SQLiteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if app.SQLite == nil {
		return nil, fmt.Errorf("%s has no SQLite database", app.Name)
	}
	sq := *app.SQLite
	d.updateApp(app.Name, func(a *state.App) { a.SQLite = nil })
	unlock := d.Deployer.Lock(app.Name)
	env, _ := d.Store.Env(app.Name)
	delete(env, sq.EnvVar)
	delete(env, state.SQLitePathVar)
	err = d.Store.SaveEnv(app.Name, env)
	unlock()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Removed %s's SQLite database from its config (%s, %s)\n", app.Name, sq.EnvVar, state.SQLitePathVar)
	res, err := d.redeployForConfig(ctx, app.Name, p.NoRestart, false, out)
	if err != nil {
		return res, err
	}
	if p.DeleteData {
		// The release that had it attached may still be draining.
		var err error
		for range 30 {
			if err = d.RT.VolumeRemove(ctx, sq.Volume); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			return res, fmt.Errorf("removing the volume %s: %w", sq.Volume, err)
		}
		fmt.Fprintf(out, "-----> Deleted the volume %s (backups in %s are kept)\n", sq.Volume, d.Store.Path("backups", "sqlite", app.Name))
	} else {
		fmt.Fprintf(out, "       The data is kept on the volume %s; `berth sqlite:add %s` attaches it again\n", sq.Volume, app.Name)
	}
	d.event(state.Event{Kind: "sqlite.remove", App: app.Name, Message: "SQLite database removed"})
	return res, nil
}

func (d *Daemon) hSQLiteBackup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SQLiteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	path, err := d.sqliteBackup(ctx, app, p.Stop, out)
	if errors.Is(err, errNeedsStop) {
		return nil, fmt.Errorf("%w; run it again with --stop", err)
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{"backup": path}, nil
}

func (d *Daemon) hSQLiteRestore(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SQLiteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if app.SQLite == nil {
		return nil, fmt.Errorf("%s has no SQLite database (berth sqlite:add %s)", app.Name, app.Name)
	}
	if err := checkSQLiteFile(ctx, p.File); err != nil {
		return nil, fmt.Errorf("%s: %w", p.File, err)
	}
	err = d.Deployer.Paused(ctx, app.Name, out, func(ctx context.Context, _ bool) error {
		return d.sqliteReplace(ctx, app, p.File, out)
	})
	if err != nil {
		return nil, err
	}
	d.event(state.Event{Kind: "sqlite.restore", App: app.Name, Message: "restored from " + filepath.Base(p.File)})
	return nil, nil
}

// sqliteReplace saves the database as it is now, then loads file in its
// place. The app must be stopped (Paused).
func (d *Daemon) sqliteReplace(ctx context.Context, app *state.App, file string, out io.Writer) error {
	dir := d.Store.Path("backups", "sqlite", app.Name)
	safety := filepath.Join(dir, time.Now().Format("20060102-150405")+"-before-restore.db")
	switch err := d.sqliteCopyStopped(ctx, app, safety); {
	case errors.Is(err, errNoSQLiteFile):
	case err != nil:
		return fmt.Errorf("saving the database before replacing it: %w", err)
	default:
		fmt.Fprintf(out, "-----> The database as it was a moment ago is in %s\n", safety)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := d.Deployer.SQLiteCopyIn(ctx, app.SQLite.Volume, f); err != nil {
		return fmt.Errorf("loading %s: %w", filepath.Base(file), err)
	}
	fmt.Fprintf(out, "-----> Loaded %s into %s's database\n", filepath.Base(file), app.Name)
	return nil
}

// sqliteBackup saves a consistent copy of app's database in
// backups/sqlite/<app>/ and prunes old ones. While the app runs, the copy
// is made inside its container with SQLite's online backup (sqlite3 or
// Python); an image with neither needs the app stopped for a few seconds,
// which only happens if allowStop. Every copy is checked on the Mac with
// PRAGMA integrity_check before it is kept.
func (d *Daemon) sqliteBackup(ctx context.Context, app *state.App, allowStop bool, out io.Writer) (string, error) {
	if app.SQLite == nil {
		return "", fmt.Errorf("%s has no SQLite database", app.Name)
	}
	// Held throughout, so a deploy can't stop the container mid-copy.
	unlock := d.Deployer.Lock(app.Name)
	defer unlock()
	dir := d.Store.Path("backups", "sqlite", app.Name)
	dest := filepath.Join(dir, time.Now().Format("20060102-150405")+".db")
	start := time.Now()
	how := "online"
	err := d.sqliteCopyOnline(ctx, app, dest)
	if errors.Is(err, errNeedsStop) || errors.Is(err, errNotRunning) {
		if errors.Is(err, errNeedsStop) && !allowStop {
			return "", err
		}
		how = "app stopped"
		err = d.Deployer.PausedLocked(ctx, app.Name, io.Discard, func(ctx context.Context, wasRunning bool) error {
			if !wasRunning {
				how = "app not running"
			}
			return d.sqliteCopyStopped(ctx, app, dest)
		})
	}
	if err != nil {
		return "", fmt.Errorf("backing up %s's SQLite database: %w", app.Name, err)
	}
	services.Prune(dir, d.cfg().PGBackupKeep)
	size := ""
	if fi, err := os.Stat(dest); err == nil {
		size = ", " + deploy.HumanBytes(fi.Size())
	}
	fmt.Fprintf(out, "-----> Backed up %s's SQLite database → %s (%s, %.1fs%s)\n", app.Name, dest, how, time.Since(start).Seconds(), size)
	return dest, nil
}

var errNotRunning = errors.New("not running")

// onlineBackupScript copies the database with SQLite's backup API (a
// consistent snapshot while the app keeps writing) and writes it to
// stdout. Exit 3: no database yet; exit 4: no sqlite3 or Python.
const onlineBackupScript = `db=` + state.SQLitePath + `; o=/tmp/.berth-backup-$$.db
[ -f "$db" ] || exit 3
if command -v sqlite3 >/dev/null 2>&1; then
  sqlite3 "$db" ".backup '$o'" >&2 || exit 1
else
  py=$(command -v python3 || command -v python) || exit 4
  "$py" -c 'import sqlite3, sys
s = sqlite3.connect(sys.argv[1]); d = sqlite3.connect(sys.argv[2])
s.backup(d); d.close(); s.close()' "$db" "$o" >&2 || exit 1
fi
cat "$o"; rc=$?; rm -f "$o"; exit $rc`

var exitCodeRe = regexp.MustCompile(`exit status (\d+)`)

// sqliteCopyOnline backs up the database from inside the running app.
func (d *Daemon) sqliteCopyOnline(ctx context.Context, app *state.App, dest string) error {
	cur := d.Store.CurrentRelease(app.Name)
	if cur == nil || cur.Container == "" {
		return errNotRunning
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	err = d.RT.Exec(ctx, cur.Container, []string{"/bin/sh", "-c", onlineBackupScript}, false, false, nil, f, &stderr)
	f.Close()
	if err != nil {
		if m := exitCodeRe.FindStringSubmatch(err.Error()); m != nil {
			switch m[1] {
			case "3":
				return errNoSQLiteFile
			case "4":
				return errNeedsStop
			}
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%v: %s", err, msg)
		}
		return err
	}
	if err := finishSQLiteCopy(ctx, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// finishSQLiteCopy checks a fresh copy and makes it one self-contained
// file: SQLite's backup API keeps the source's WAL mode in the copy's
// header, and a WAL database can't be opened read-only without its -shm.
func finishSQLiteCopy(ctx context.Context, path string) error {
	if err := checkSQLiteFile(ctx, path); err != nil {
		return fmt.Errorf("the copy failed its check: %w", err)
	}
	if o, err := exec.CommandContext(ctx, "sqlite3", path, "PRAGMA journal_mode=DELETE").CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(string(o)))
	}
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	os.Chmod(path, 0o600)
	return nil
}

// sqliteCopyStopped copies the database files out of the volume with a
// helper container (the app must be stopped), then has the Mac's sqlite3
// fold any -wal file in and write one clean, checked file.
func (d *Daemon) sqliteCopyStopped(ctx context.Context, app *state.App, dest string) error {
	if err := os.MkdirAll(d.Store.Path("backups"), 0o700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(d.Store.Path("backups"), ".sqlite-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := d.Deployer.SQLiteCopyOut(ctx, app.SQLite.Volume, pw)
		pw.CloseWithError(err)
		copied <- err
	}()
	untarErr := untarSQLite(pr, work)
	pr.CloseWithError(untarErr)
	if err := <-copied; err != nil {
		if m := exitCodeRe.FindStringSubmatch(err.Error()); m != nil && m[1] == "3" {
			return errNoSQLiteFile
		}
		return err
	}
	if untarErr != nil {
		return fmt.Errorf("reading the copy: %w", untarErr)
	}
	src := filepath.Join(work, state.SQLiteFile)
	if _, err := os.Stat(src); err != nil {
		return errNoSQLiteFile
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	os.Remove(tmp)
	defer os.Remove(tmp)
	if o, err := exec.CommandContext(ctx, "sqlite3", src, ".backup '"+tmp+"'").CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3 .backup: %v: %s", err, strings.TrimSpace(string(o)))
	}
	if err := finishSQLiteCopy(ctx, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// untarSQLite writes the database files (app.db, app.db-wal) from a tar
// stream into dir.
func untarSQLite(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Base(h.Name)
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(name, state.SQLiteFile) {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
	}
}

// checkSQLiteFile runs PRAGMA integrity_check with the Mac's sqlite3.
func checkSQLiteFile(ctx context.Context, path string) error {
	head := make([]byte, 16)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(f, head)
	f.Close()
	if err != nil || string(head) != "SQLite format 3\x00" {
		return fmt.Errorf("not a SQLite database")
	}
	// immutable: no locks and no -shm, so a WAL-mode file can be checked
	// read-only and nothing is written beside it.
	uri := "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path) + "?immutable=1"
	o, err := exec.CommandContext(ctx, "sqlite3", "-readonly", uri, "PRAGMA integrity_check").CombinedOutput()
	if res := strings.TrimSpace(string(o)); err != nil || res != "ok" {
		if res == "" && err != nil {
			res = err.Error()
		}
		return fmt.Errorf("integrity check: %s", firstLine(res))
	}
	return nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// sqliteReleaseBackup copies the database before a release command runs.
// The deploy has already stopped the app (one container at a time), so
// it is a plain copy, and works for any image.
func (d *Daemon) sqliteReleaseBackup(ctx context.Context, app *state.App, rel *state.Release, out io.Writer) (string, error) {
	dir := d.Store.Path("backups", "sqlite", app.Name, "releases")
	dest := filepath.Join(dir, fmt.Sprintf("%s-v%d.db", time.Now().Format("20060102-150405"), rel.N))
	start := time.Now()
	if err := d.sqliteCopyStopped(ctx, app, dest); err != nil {
		return "", err
	}
	services.Prune(dir, d.cfg().PGBackupKeep)
	size := ""
	if fi, err := os.Stat(dest); err == nil {
		size = ", " + deploy.HumanBytes(fi.Size())
	}
	fmt.Fprintf(out, "-----> Backed up the SQLite database before the release command (%.1fs%s)\n", time.Since(start).Seconds(), size)
	return dest, nil
}

// latestSQLiteBackup is the newest nightly or manual backup, or "".
func (d *Daemon) latestSQLiteBackup(app string) string {
	entries, _ := os.ReadDir(d.Store.Path("backups", "sqlite", app))
	latest := ""
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".db") && n > latest {
			latest = n
		}
	}
	if latest == "" {
		return ""
	}
	return d.Store.Path("backups", "sqlite", app, latest)
}

// sqliteApps lists production apps with SQLite (dev versions and
// previews hold copies made for trying things, and aren't backed up).
func (d *Daemon) sqliteApps() []*state.App {
	apps, _ := d.Store.Apps()
	var out []*state.App
	for _, a := range apps {
		if a.SQLite != nil && a.DevOf == "" {
			out = append(out, a)
		}
	}
	return out
}

// backupAllSQLite backs up every production app's database (nightly, berth
// backup, off-Mac). allowStop lets an image without sqlite3 or Python be
// stopped for a few seconds; otherwise it is skipped with a warning.
func (d *Daemon) backupAllSQLite(ctx context.Context, allowStop bool, out io.Writer) {
	for _, a := range d.sqliteApps() {
		path, err := d.sqliteBackup(ctx, a, allowStop, out)
		switch {
		case errors.Is(err, errNoSQLiteFile):
			continue
		case err != nil:
			fmt.Fprintf(out, "       warning: %v\n", err)
			d.event(state.Event{Kind: "backup.failed", App: a.Name, Message: err.Error()})
		default:
			d.event(state.Event{Kind: "backup", App: a.Name, Message: "sqlite → " + filepath.Base(path)})
		}
	}
}

// SQLiteInfo is `berth sqlite:list`'s row.
type SQLiteInfo struct {
	App        string `json:"app"`
	Volume     string `json:"volume"`
	EnvVar     string `json:"env_var"`
	DevOf      string `json:"dev_of,omitempty"`
	LastBackup string `json:"last_backup,omitempty"`
}

func (d *Daemon) hSQLiteList(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	apps, _ := d.Store.Apps()
	list := []SQLiteInfo{}
	for _, a := range apps {
		if a.SQLite != nil {
			list = append(list, SQLiteInfo{App: a.Name, Volume: a.SQLite.Volume, EnvVar: a.SQLite.EnvVar, DevOf: a.DevOf, LastBackup: d.latestSQLiteBackup(a.Name)})
		}
	}
	return list, nil
}

// restoreSQLiteVolume recreates an app's SQLite volume on a new Mac
// (berth restore) and loads its newest backup into it, if the volume
// wasn't there already.
func (d *Daemon) restoreSQLiteVolume(ctx context.Context, a *state.App, out io.Writer) error {
	fresh := !d.RT.VolumeExists(ctx, a.SQLite.Volume)
	if err := d.Deployer.SQLitePrepare(ctx, a.SQLite.Volume); err != nil {
		return fmt.Errorf("%s's SQLite volume: %w", a.Name, err)
	}
	backup := d.latestSQLiteBackup(a.Name)
	if !fresh || backup == "" || a.DevOf != "" {
		return nil
	}
	fmt.Fprintf(out, "-----> Loading %s into %s's SQLite database\n", filepath.Base(backup), a.Name)
	f, err := os.Open(backup)
	if err != nil {
		return err
	}
	defer f.Close()
	return d.Deployer.SQLiteCopyIn(ctx, a.SQLite.Volume, f)
}

// sqliteTryDuration is how long Caddy holds a request for an app with
// SQLite while its old release has stopped and the new one is starting
// (VM boot, release command, health check). Cloudflare waits 100 s.
const sqliteTryDuration = "60s"
