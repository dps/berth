package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// sqliteFile makes a real SQLite database with the Mac's sqlite3.
func sqliteFile(t *testing.T, sql string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.db")
	if out, err := exec.Command("sqlite3", p, sql).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v %s", err, out)
	}
	b, _ := os.ReadFile(p)
	return b
}

func sqliteQuery(t *testing.T, path, sql string) string {
	t.Helper()
	out, err := exec.Command("sqlite3", path, sql).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %s: %v %s", path, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newSQLiteApp(t *testing.T) (*Daemon, *runtime.Fake) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("needs sqlite3")
	}
	d, rt, repo := newTestDaemon(t)
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	b, _ = json.Marshal(SQLiteParams{App: "web"})
	if _, err := d.hSQLiteAdd(d.ctx, b, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return d, rt
}

func TestSQLiteAdd(t *testing.T) {
	d, rt := newSQLiteApp(t)
	app, _ := d.Store.App("web")
	if app.SQLite == nil || app.SQLite.Volume != "berth-sqlite-web" || app.SQLite.EnvVar != "DATABASE_URL" {
		t.Fatalf("app %+v", app.SQLite)
	}
	if !rt.VolumeExists(d.ctx, "berth-sqlite-web") || len(rt.CallLog("runwith alpine:3 chmod 0777 /data")) != 1 {
		t.Fatalf("volume not prepared: %v", rt.CallLog(""))
	}
	cur := d.Store.CurrentRelease("web")
	if cur.N != 2 || cur.Env["DATABASE_URL"] != "sqlite:////data/sqlite/app.db" || cur.Env["SQLITE_PATH"] != "/data/sqlite/app.db" {
		t.Fatalf("release %d env %v", cur.N, cur.Env)
	}
	o, _ := rt.Opts(cur.Container)
	if strings.Join(o.Volumes, ",") != "berth-sqlite-web:/data/sqlite" {
		t.Fatalf("volumes %v", o.Volumes)
	}
	// A second add, or one that would clobber a linked database's URL, is refused.
	b, _ := json.Marshal(SQLiteParams{App: "web"})
	if _, err := d.hSQLiteAdd(d.ctx, b, io.Discard); err == nil {
		t.Fatal("added SQLite twice")
	}
	// Caddy waits longer for it.
	d.Apply(d.ctx)
	conf, _ := os.ReadFile(d.Store.Path("caddy.json"))
	if !strings.Contains(string(conf), `"try_duration": "`+sqliteTryDuration+`"`) {
		t.Fatal("route doesn't wait out the stop-first gap")
	}

	var out bytes.Buffer
	b, _ = json.Marshal(SQLiteParams{App: "web"})
	if _, err := d.hSQLiteRemove(d.ctx, b, &out); err != nil {
		t.Fatal(err)
	}
	cur = d.Store.CurrentRelease("web")
	if _, ok := cur.Env["DATABASE_URL"]; ok || !rt.VolumeExists(d.ctx, "berth-sqlite-web") {
		t.Fatalf("after remove: env %v", cur.Env)
	}
	if !strings.Contains(out.String(), "kept on the volume") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestSQLiteAddRefusesATakenVariable(t *testing.T) {
	d, _, repo := newTestDaemon(t)
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo, NoDeploy: true})
	d.hCreate(d.ctx, b, io.Discard)
	env, _ := d.Store.Env("web")
	env["DATABASE_URL"] = "postgres://x"
	d.Store.SaveEnv("web", env)
	b, _ = json.Marshal(SQLiteParams{App: "web"})
	if _, err := d.hSQLiteAdd(d.ctx, b, io.Discard); err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("want a --as hint, got %v", err)
	}
	b, _ = json.Marshal(SQLiteParams{App: "web", As: "SQLITE_URL", NoRestart: true})
	if _, err := d.hSQLiteAdd(d.ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	env, _ = d.Store.Env("web")
	if env["DATABASE_URL"] != "postgres://x" || env["SQLITE_URL"] != state.SQLiteURL {
		t.Fatalf("env %v", env)
	}
}

func TestSQLiteBackupWhileRunning(t *testing.T) {
	d, rt := newSQLiteApp(t)
	db := sqliteFile(t, "create table notes(body); insert into notes values ('hello');")
	var ran string
	rt.ExecHook = func(name string, args []string, stdout io.Writer) error {
		ran = name
		stdout.Write(db)
		return nil
	}
	b, _ := json.Marshal(SQLiteParams{App: "web"})
	res, err := d.hSQLiteBackup(d.ctx, b, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := res.(map[string]string)["backup"]
	if ran != d.Store.CurrentRelease("web").Container {
		t.Fatalf("backup ran in %q", ran)
	}
	if got := sqliteQuery(t, path, "select body from notes"); got != "hello" {
		t.Fatalf("backup has %q", got)
	}
	if !strings.HasPrefix(path, d.Store.Path("backups", "sqlite", "web")+"/") || d.latestSQLiteBackup("web") != path {
		t.Fatalf("backup at %s", path)
	}
	// A copy that isn't a sound database is not kept.
	rt.ExecHook = func(name string, args []string, stdout io.Writer) error {
		stdout.Write(db[:len(db)/2])
		return nil
	}
	if _, err := d.hSQLiteBackup(d.ctx, b, io.Discard); err == nil {
		t.Fatal("kept a torn copy")
	}
}

// tarOf is what the helper container streams out of the volume.
func tarOf(files map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	return buf.Bytes()
}

func TestSQLiteBackupWithoutToolsStopsTheApp(t *testing.T) {
	d, rt := newSQLiteApp(t)
	before := d.Store.CurrentRelease("web").Container
	db := sqliteFile(t, "create table t(x); insert into t values (42);")
	rt.ExecHook = func(name string, args []string, stdout io.Writer) error {
		return &exitErr{4} // no sqlite3, no Python
	}
	rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error {
		if o.Image == "alpine:3" && strings.Contains(strings.Join(o.Args, " "), "tar -cf") {
			stdout.Write(tarOf(map[string][]byte{"app.db": db}))
		}
		return nil
	}
	b, _ := json.Marshal(SQLiteParams{App: "web"})
	if _, err := d.hSQLiteBackup(d.ctx, b, io.Discard); err == nil || !strings.Contains(err.Error(), "--stop") {
		t.Fatalf("without --stop: %v", err)
	}
	if d.Store.CurrentRelease("web").Container != before {
		t.Fatal("stopped the app without --stop")
	}
	b, _ = json.Marshal(SQLiteParams{App: "web", Stop: true})
	res, err := d.hSQLiteBackup(d.ctx, b, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := sqliteQuery(t, res.(map[string]string)["backup"], "select x from t"); got != "42" {
		t.Fatalf("backup has %q", got)
	}
	cur := d.Store.CurrentRelease("web")
	if cur.Container == before || cur.Status != state.StatusRunning {
		t.Fatalf("the app should be running again in a new container: %+v", cur)
	}
}

type exitErr struct{ code int }

func (e *exitErr) Error() string { return "exit status " + string(rune('0'+e.code)) }

func TestSQLiteRestore(t *testing.T) {
	d, rt := newSQLiteApp(t)
	before := d.Store.CurrentRelease("web").Container
	old := sqliteFile(t, "create table t(x); insert into t values ('old');")
	rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error {
		if strings.Contains(strings.Join(o.Args, " "), "tar -cf") {
			stdout.Write(tarOf(map[string][]byte{"app.db": old}))
		}
		return nil
	}
	newer := filepath.Join(t.TempDir(), "new.db")
	os.WriteFile(newer, sqliteFile(t, "create table t(x); insert into t values ('new');"), 0o600)
	bad := filepath.Join(t.TempDir(), "bad.db")
	os.WriteFile(bad, []byte("not a database"), 0o600)
	b, _ := json.Marshal(SQLiteParams{App: "web", File: bad})
	if _, err := d.hSQLiteRestore(d.ctx, b, io.Discard); err == nil {
		t.Fatal("restored a file that isn't a database")
	}
	var out bytes.Buffer
	b, _ = json.Marshal(SQLiteParams{App: "web", File: newer})
	if _, err := d.hSQLiteRestore(d.ctx, b, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	calls := rt.CallLog("")
	stop, load := -1, -1
	for i, c := range calls {
		if strings.HasPrefix(c, "stop "+before) && stop < 0 {
			stop = i
		}
		if strings.Contains(c, "cat > .restore.tmp") {
			load = i
		}
	}
	if stop < 0 || load < stop {
		t.Fatalf("must stop the app before loading: %v", calls)
	}
	// The database as it was is saved first.
	matches, _ := filepath.Glob(d.Store.Path("backups", "sqlite", "web", "*-before-restore.db"))
	if len(matches) != 1 || sqliteQuery(t, matches[0], "select x from t") != "old" {
		t.Fatalf("safety copy: %v\n%s", matches, out.String())
	}
	if cur := d.Store.CurrentRelease("web"); cur.Container == before || cur.Status != state.StatusRunning {
		t.Fatalf("app after restore: %+v", cur)
	}
}

func TestSQLiteDevVersionGetsItsOwn(t *testing.T) {
	d, rt := newSQLiteApp(t)
	parent, _ := d.Store.App("web")
	dev, err := d.ensureChild(d.ctx, parent, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if dev.SQLite == nil || dev.SQLite.Volume != "berth-sqlite-web-dev" || !rt.VolumeExists(context.Background(), "berth-sqlite-web-dev") {
		t.Fatalf("dev %+v", dev.SQLite)
	}
	env, _ := d.Store.Env(dev.Name)
	if env["DATABASE_URL"] != state.SQLiteURL {
		t.Fatalf("dev env %v", env)
	}
	d.destroyApp(d.ctx, dev, false, io.Discard)
	if rt.VolumeExists(context.Background(), "berth-sqlite-web-dev") {
		t.Fatal("destroying the dev version kept its volume")
	}
	if !rt.VolumeExists(context.Background(), "berth-sqlite-web") {
		t.Fatal("destroying the dev version removed production's volume")
	}
}
