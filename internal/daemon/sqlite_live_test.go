package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// TestLiveSQLite runs the SQLite backup and restore paths against Apple's
// container with throwaway containers and a throwaway volume, in a
// temporary Berth home (BERTH_LIVE=1; needs python:3-alpine and alpine:3,
// pulled if missing). It doesn't touch the local Berth's apps.
func TestLiveSQLite(t *testing.T) {
	if os.Getenv("BERTH_LIVE") == "" {
		t.Skip("BERTH_LIVE not set")
	}
	ctx := context.Background()
	s := &state.Store{Root: t.TempDir()}
	cfg := state.DefaultConfig()
	s.SaveConfig(cfg)
	rt := runtime.NewApple()
	d := New(s, rt)
	suffix := strconv.FormatInt(time.Now().UnixMilli()%1_000_000, 36)
	app := "sqlitelive" + suffix
	vol := state.SQLiteVolume(app)
	writer := "berth-" + app + "-writer"
	cleanup := func() {
		c := context.Background()
		for _, n := range []string{writer, writer + "-2", writer + "-bare"} {
			rt.Remove(c, n)
		}
		rt.VolumeRemove(c, vol)
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := d.Deployer.SQLitePrepare(ctx, vol); err != nil {
		t.Fatal(err)
	}
	a := &state.App{Name: app, SQLite: &state.SQLite{Volume: vol, EnvVar: "DATABASE_URL"}, CreatedAt: time.Now()}
	s.SaveApp(a)

	// An app that writes a row every few milliseconds, in WAL mode, as a
	// user that isn't root.
	script := `import sqlite3, time, os
os.setgid(1000); os.setuid(1000)
c = sqlite3.connect("` + state.SQLitePath + `", isolation_level=None)
c.execute("pragma journal_mode=wal")
c.execute("create table if not exists t(id integer primary key, ts real, body text)")
while True:
    c.execute("insert into t(ts, body) values (?, ?)", (time.time(), "x" * 500))
    time.sleep(0.002)
`
	if err := rt.Run(ctx, runtime.RunOpts{Name: writer, Image: "python:3-alpine", Volumes: []string{vol + ":" + state.SQLiteDir},
		Entrypoint: "python", Args: []string{"-c", script}, Detach: true}); err != nil {
		t.Fatal(err)
	}
	s.SaveRelease(app, &state.Release{N: 1, Image: "python:3-alpine", Container: writer, Status: state.StatusRunning, StartedAt: time.Now()})
	s.SetCurrent(app, 1)
	time.Sleep(3 * time.Second)

	// While it runs, a second VM can't attach the volume.
	err := rt.RunWith(ctx, runtime.RunOpts{Name: writer + "-2", Image: "alpine:3", Remove: true, Volumes: []string{vol + ":/data"}, Args: []string{"true"}}, nil, io.Discard)
	if err == nil {
		t.Fatal("a second container attached the SQLite volume")
	}
	t.Logf("second attach refused: %v", firstLine(err.Error()))

	// Online backups while it keeps writing, each a consistent snapshot.
	var counts []int
	for i := range 3 {
		dest := filepath.Join(s.Path("backups", "sqlite", app), fmt.Sprintf("online-%d.db", i))
		if err := d.sqliteCopyOnline(ctx, a, dest); err != nil {
			t.Fatalf("online backup %d: %v", i, err)
		}
		n, _ := strconv.Atoi(sqliteQuery(t, dest, "select count(*) from t"))
		counts = append(counts, n)
		time.Sleep(500 * time.Millisecond)
	}
	if counts[0] == 0 || counts[2] <= counts[0] {
		t.Fatalf("row counts across backups: %v", counts)
	}
	t.Logf("online backups while writing: %v rows", counts)

	// Stopped: the helper copies the database and its -wal file out, and
	// the Mac folds them into one file.
	rt.Stop(ctx, writer, 5*time.Second)
	rt.Remove(ctx, writer)
	var ls bytes.Buffer
	rt.RunWith(ctx, runtime.RunOpts{Image: "alpine:3", Remove: true, Volumes: []string{vol + ":/data"}, Args: []string{"sh", "-c", "ls -ln /data"}}, nil, &ls)
	t.Logf("volume after stop:\n%s", ls.String())
	stopped := filepath.Join(s.Path("backups", "sqlite", app), "stopped.db")
	if err := d.sqliteCopyStopped(ctx, a, stopped); err != nil {
		t.Fatal(err)
	}
	final, _ := strconv.Atoi(sqliteQuery(t, stopped, "select count(*) from t"))
	if final < counts[2] {
		t.Fatalf("stopped copy has %d rows, fewer than the last online one (%d)", final, counts[2])
	}
	t.Logf("stopped copy: %d rows", final)

	// Restore an older backup; the file keeps the app user's ownership.
	f, _ := os.Open(filepath.Join(s.Path("backups", "sqlite", app), "online-0.db"))
	err = d.Deployer.SQLiteCopyIn(ctx, vol, f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	ls.Reset()
	rt.RunWith(ctx, runtime.RunOpts{Image: "alpine:3", Remove: true, Volumes: []string{vol + ":/data"}, Args: []string{"sh", "-c", "ls -ln /data; stat -c '%u:%g' /data/" + state.SQLiteFile}}, nil, &ls)
	if !strings.Contains(ls.String(), "1000:1000") || strings.Contains(ls.String(), state.SQLiteFile+"-wal") {
		t.Fatalf("after restore:\n%s", ls.String())
	}
	back := filepath.Join(t.TempDir(), "back.db")
	if err := d.sqliteCopyStopped(ctx, a, back); err != nil {
		t.Fatal(err)
	}
	if n, _ := strconv.Atoi(sqliteQuery(t, back, "select count(*) from t")); n != counts[0] {
		t.Fatalf("restored database has %d rows, want %d", n, counts[0])
	}

	// An image with neither sqlite3 nor Python: online backup says so.
	bare := writer + "-bare"
	if err := rt.Run(ctx, runtime.RunOpts{Name: bare, Image: "alpine:3", Volumes: []string{vol + ":" + state.SQLiteDir}, Args: []string{"sleep", "60"}, Detach: true}); err != nil {
		t.Fatal(err)
	}
	s.SaveRelease(app, &state.Release{N: 1, Image: "alpine:3", Container: bare, Status: state.StatusRunning, StartedAt: time.Now()})
	err = d.sqliteCopyOnline(ctx, a, filepath.Join(t.TempDir(), "x.db"))
	if !errors.Is(err, errNeedsStop) {
		t.Fatalf("bare image: %v", err)
	}
}
