package daemon

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// TestLiveWorkers runs workers inside a real web container: their output
// reaches app.log, a scale-down stops one with TERM, and a worker left
// behind by an exec client killed outright (a berthd that died) is replaced
// rather than doubled.
func TestLiveWorkers(t *testing.T) {
	if os.Getenv("BERTH_LIVE") == "" {
		t.Skip("BERTH_LIVE not set")
	}
	ctx := context.Background()
	s := &state.Store{Root: t.TempDir()}
	s.SaveConfig(state.DefaultConfig())
	rt := runtime.NewApple()
	d := New(s, rt)
	d.ctx = ctx
	app := "inworkers" + strconv.FormatInt(time.Now().UnixMilli()%1_000_000, 36)
	web := "berth-" + app + "-web"
	t.Cleanup(func() { rt.Remove(context.Background(), web) })
	if err := rt.Run(ctx, runtime.RunOpts{Name: web, Image: "python:3-alpine", Args: []string{"sleep", "600"}, Detach: true, Init: true}); err != nil {
		t.Fatal(err)
	}
	worker := `import os, signal, sys, time
signal.signal(signal.SIGTERM, lambda *_: (print(os.environ["BERTH_PROCESS"], "got TERM", flush=True), sys.exit(0)))
print(os.environ["BERTH_PROCESS"], "up as", os.getuid(), flush=True)
while True: time.sleep(1)`
	a := &state.App{Name: app, Scale: map[string]int{"worker": 2}, CreatedAt: time.Now()}
	s.SaveApp(a)
	s.SaveRelease(app, &state.Release{N: 1, Container: web, Status: state.StatusRunning, Processes: map[string]string{"worker": "exec python -c '" + worker + "'"}})
	s.SetCurrent(app, 1)
	sync := func() {
		apps, _ := s.Apps()
		cs, _ := rt.List(ctx)
		d.syncAllWorkers(ctx, apps, cs)
	}
	procs := func() string {
		var b bytes.Buffer
		rt.Exec(ctx, web, []string{"sh", "-c", "grep -l BERTH_PROCESS= /proc/[0-9]*/environ 2>/dev/null | while read e; do p=${e#/proc/}; p=${p%/environ}; tr '\\0' '\\n' < $e | grep BERTH_PROCESS=; done | sort"}, false, false, nil, &b, io.Discard)
		return strings.TrimSpace(b.String())
	}
	logHas := func(s2 string) bool {
		b, _ := os.ReadFile(s.Path("apps", app, "logs", "app.log"))
		return strings.Contains(string(b), s2)
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for range 60 {
			if ok() {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		b, _ := os.ReadFile(s.Path("apps", app, "logs", "app.log"))
		t.Fatalf("timed out waiting for %s; processes:\n%s\napp.log:\n%s", what, procs(), b)
	}

	sync()
	wait("two workers", func() bool { return procs() == "BERTH_PROCESS=worker.1\nBERTH_PROCESS=worker.2" })
	wait("their output", func() bool { return logHas("v1 worker.1: worker.1 up as") && logHas("v1 worker.2: worker.2 up as") })

	// Scale down: worker.2 gets TERM and goes.
	a.Scale["worker"] = 1
	s.SaveApp(a)
	sync()
	wait("worker.2 to stop", func() bool { return procs() == "BERTH_PROCESS=worker.1" })
	wait("worker.2's TERM", func() bool { return logHas("worker.2: worker.2 got TERM") })
	time.Sleep(3 * time.Second)
	if logHas("worker.2: [berth]") || procs() != "BERTH_PROCESS=worker.1" {
		t.Fatalf("worker.2 was restarted after being scaled away: %q", procs())
	}

	// A berthd that dies: its exec clients are killed outright and the
	// worker carries on inside (its output now goes nowhere). A new berthd
	// replaces it rather than adding a second.
	d.wp.mu.Lock()
	w1 := d.wp.m[app+"/worker.1"]
	d.wp.m = nil
	d.wp.mu.Unlock()
	w1.cancel() // SIGKILLs the exec client
	<-w1.done
	if procs() != "BERTH_PROCESS=worker.1" {
		t.Fatalf("after the client was killed: %q", procs())
	}
	d2 := New(s, rt)
	d2.ctx = ctx
	apps, _ := s.Apps()
	cs, _ := rt.List(ctx)
	d2.syncAllWorkers(ctx, apps, cs)
	wait("worker.1 started again", func() bool {
		b, _ := os.ReadFile(s.Path("apps", app, "logs", "app.log"))
		return strings.Count(string(b), "worker.1: worker.1 up") == 2
	})
	time.Sleep(2 * time.Second)
	if got := procs(); got != "BERTH_PROCESS=worker.1" {
		t.Fatalf("after a new berthd: %q", got)
	}
	d2.stopAllWorkers()
	if got := procs(); got != "" {
		t.Fatalf("after stop: %q", got)
	}
	b, _ := os.ReadFile(s.Path("apps", app, "logs", "app.log"))
	t.Logf("app.log:\n%s", b)
}
