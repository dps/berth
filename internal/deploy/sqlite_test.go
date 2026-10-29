package deploy

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// newSQLiteHarness is a harness whose app has a SQLite database. The fake
// runtime refuses to attach the volume to a second running container, as
// Apple's does, so any overlap fails the test.
func newSQLiteHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	app, _ := h.store.App("web")
	app.SQLite = &state.SQLite{Volume: state.SQLiteVolume("web"), EnvVar: "DATABASE_URL"}
	h.store.SaveApp(app)
	return h
}

func callIndex(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestSQLiteDeployStopsTheOldReleaseFirst(t *testing.T) {
	h := newSQLiteHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	h.commit(map[string]string{"x": "2"})
	var out bytes.Buffer
	r2, err := h.d.Deploy(ctx, Request{App: "web", Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	calls := h.rt.CallLog("")
	if s, r := callIndex(calls, "stop "+r1.Container), callIndex(calls, "run "+r2.Container); s < 0 || s > r {
		t.Fatalf("v1 must stop before v2 runs: %v", calls)
	}
	if !strings.Contains(out.String(), "Stopping v1 first") {
		t.Errorf("output should say why v1 stopped first:\n%s", out.String())
	}
	old, _ := h.store.Release("web", 1)
	if old.Status != state.StatusStopped || old.Container != "" {
		t.Fatalf("v1 after the swap: %+v", old)
	}
	running(t, h, r2.Container)
	gone(t, h, r1.Container)
	o, _ := h.rt.Opts(r2.Container)
	vols := o.Volumes
	if len(vols) != 1 || vols[0] != "berth-sqlite-web:"+state.SQLiteDir {
		t.Fatalf("volumes %v", vols)
	}
}

func TestSQLiteReleaseCommandRunsWithTheAppStopped(t *testing.T) {
	h := newSQLiteHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	h.commit(map[string]string{"Procfile": "web: ./server\nrelease: ./migrate up\n"})
	r2, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	calls := h.rt.CallLog("")
	stop, rel, run := callIndex(calls, "stop "+r1.Container), callIndex(calls, "runwith berth/web:2 -c ./migrate up"), callIndex(calls, "run "+r2.Container)
	if stop < 0 || !(stop < rel && rel < run) {
		t.Fatalf("want stop v1, release command, run v2: %v", calls)
	}
}

func TestSQLiteFailedDeployStartsTheOldReleaseAgain(t *testing.T) {
	h := newSQLiteHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.BuildHook = func(o runtime.BuildOpts) (string, error) { return "unhealthy", nil }
	h.commit(map[string]string{"x": "bad"})
	var out bytes.Buffer
	if _, err := h.d.Deploy(ctx, Request{App: "web", Out: &out}); err == nil {
		t.Fatal("an unhealthy release deployed")
	}
	if h.store.Current("web") != 1 {
		t.Fatal("current moved off v1")
	}
	cur := h.store.CurrentRelease("web")
	if cur.Container == "" || cur.Container == r1.Container || cur.Status != state.StatusRunning {
		t.Fatalf("v1 should run again in a new container: %+v", cur)
	}
	running(t, h, cur.Container)
	if !strings.Contains(get(t, cur.HostPort), cur.Container) {
		t.Fatal("v1 isn't serving")
	}
	if s := out.String(); !strings.Contains(s, "Starting v1 again") || !strings.Contains(s, "v1 is serving traffic again") || strings.Contains(s, "still serving") {
		t.Fatalf("output:\n%s", s)
	}

	// A release command that fails brings v1 back too.
	h.rt.BuildHook = nil
	h.rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error { return io.ErrUnexpectedEOF }
	h.commit(map[string]string{"Procfile": "release: ./migrate up\n"})
	out.Reset()
	if _, err := h.d.Deploy(ctx, Request{App: "web", Out: &out}); err == nil {
		t.Fatal("a failed release command deployed")
	}
	again := h.store.CurrentRelease("web")
	if again.N != 1 || again.Container == "" || again.Container == cur.Container {
		t.Fatalf("v1 after the failed release command: %+v", again)
	}
	running(t, h, again.Container)
}

func TestSQLiteRestartAndRollback(t *testing.T) {
	h := newSQLiteHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Restart(ctx, "web", io.Discard); err != nil {
		t.Fatal(err)
	}
	cur := h.store.CurrentRelease("web")
	calls := h.rt.CallLog("")
	if s, r := callIndex(calls, "stop "+r1.Container), callIndex(calls, "run "+cur.Container); s < 0 || s > r {
		t.Fatalf("restart must stop first: %v", calls)
	}
	running(t, h, cur.Container)
	gone(t, h, r1.Container)

	h.commit(map[string]string{"x": "2"})
	if _, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	back, err := h.d.Rollback(ctx, "web", 0, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if back.N != 1 || h.store.Current("web") != 1 {
		t.Fatalf("rolled back to %d", back.N)
	}
	running(t, h, back.Container)
	if two, _ := h.store.Release("web", 2); two.Container != "" {
		t.Fatalf("v2 still has a container: %+v", two)
	}
}

func TestSQLiteCronRunsInside(t *testing.T) {
	h := newSQLiteHarness(t)
	ctx := context.Background()
	h.commit(map[string]string{
		"Procfile": "web: ./server\nworker: ./work\n",
		"app.json": `{"cron":[{"command":"./digest","schedule":"@daily"}]}`,
	})
	var out bytes.Buffer
	rel, err := h.d.Deploy(ctx, Request{App: "web", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	h.rt.ExecHook = func(name string, args []string, stdout io.Writer) error {
		ran = append([]string{name}, args...)
		io.WriteString(stdout, "sent\n")
		return nil
	}
	var jobOut bytes.Buffer
	if err := h.d.RunJob(ctx, "web", rel, 1, &jobOut); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ran, " ") != rel.Container+" /bin/sh -c ./digest" || jobOut.String() != "sent\n" {
		t.Fatalf("ran %v, output %q", ran, jobOut.String())
	}
	if len(h.rt.CallLog("runwith")) != 0 {
		t.Fatal("the job ran in a new container")
	}
	h.d.Stop(ctx, "web")
	if err := h.d.RunJob(ctx, "web", rel, 1, io.Discard); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Fatalf("job on a stopped app: %v", err)
	}
}

func TestSQLitePaused(t *testing.T) {
	h := newSQLiteHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	var sawRunning bool
	err = h.d.Paused(ctx, "web", io.Discard, func(ctx context.Context, wasRunning bool) error {
		sawRunning = wasRunning
		// The volume is free: a helper can attach it.
		return h.d.SQLiteCopyOut(ctx, state.SQLiteVolume("web"), io.Discard)
	})
	if err != nil || !sawRunning {
		t.Fatalf("paused: %v (was running: %v)", err, sawRunning)
	}
	cur := h.store.CurrentRelease("web")
	if cur.Container == r1.Container || cur.Status != state.StatusRunning {
		t.Fatalf("after the pause: %+v", cur)
	}
	running(t, h, cur.Container)
	// While the app runs, a helper can't attach the volume.
	if err := h.d.SQLiteCopyOut(ctx, state.SQLiteVolume("web"), io.Discard); err == nil {
		t.Fatal("attached the volume to a second VM")
	}
}
