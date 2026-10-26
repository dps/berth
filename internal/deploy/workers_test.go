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

func running(t *testing.T, h *harness, names ...string) {
	t.Helper()
	cs, _ := h.rt.List(context.Background())
	for _, n := range names {
		if c, ok := runtime.Find(cs, n); !ok || !c.Running() {
			t.Fatalf("%s isn't running", n)
		}
	}
}

func gone(t *testing.T, h *harness, names ...string) {
	t.Helper()
	cs, _ := h.rt.List(context.Background())
	for _, n := range names {
		if _, ok := runtime.Find(cs, n); ok {
			t.Fatalf("%s is still there", n)
		}
	}
}

func TestWorkersFollowTheRelease(t *testing.T) {
	workerSettle = 0
	h := newHarness(t)
	ctx := context.Background()
	h.commit(map[string]string{
		"Procfile": "web: node server.js\nrelease: node migrate.js\nworker: node worker.js\nclock: node clock.js\n",
		"app.json": `{"cron":[{"command":"node digest.js","schedule":"0 7 * * *"}]}`,
	})
	var out bytes.Buffer
	r1, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test", Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(r1.Workers) != 2 || r1.Processes["worker"] != "node worker.js" || len(r1.Cron) != 1 || r1.Cron[0].Schedule != "0 7 * * *" {
		t.Fatalf("v1 workers %+v processes %v cron %v", r1.Workers, r1.Processes, r1.Cron)
	}
	if !strings.Contains(out.String(), "Started clock.1, worker.1") {
		t.Fatalf("deploy output:\n%s", out.String())
	}
	v1 := []string{r1.Workers[0].Container, r1.Workers[1].Container}
	running(t, h, v1...)
	cs, _ := h.rt.List(ctx)
	if c, _ := runtime.Find(cs, v1[0]); c.Labels["dev.berth.process"] != "clock.1" || c.Labels["dev.berth.app"] != "web" {
		t.Fatalf("labels %v", c.Labels)
	}

	// A new release replaces the workers.
	h.commit(map[string]string{"x.txt": "2"})
	r2, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	gone(t, h, v1...)
	if old, _ := h.store.Release("web", 1); len(old.Workers) != 0 {
		t.Fatalf("v1 still lists workers %+v", old.Workers)
	}
	running(t, h, r2.Workers[0].Container, r2.Workers[1].Container)

	// Config changes keep the Procfile's processes and the cron list.
	h.store.SaveEnv("web", state.Env{"BERTH_DRAIN": "0", "BERTH_HEALTH_TIMEOUT": "3", "A": "1"})
	r3, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "config", ReuseImage: true, Out: &out})
	if err != nil || len(r3.Workers) != 2 || len(r3.Cron) != 1 {
		t.Fatalf("config release: %v %+v", err, r3)
	}

	// Scale.
	if err := h.d.Scale(ctx, "web", "worker", 3, &out); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Scale(ctx, "web", "clock", 0, &out); err != nil {
		t.Fatal(err)
	}
	cur := h.store.CurrentRelease("web")
	var names []string
	for _, w := range cur.Workers {
		names = append(names, w.Name())
	}
	if strings.Join(names, " ") != "worker.1 worker.2 worker.3" {
		t.Fatalf("after scaling: %v", names)
	}
	if err := h.d.Scale(ctx, "web", "nope", 1, &out); err == nil || !strings.Contains(err.Error(), "clock, worker") {
		t.Fatalf("unknown type: %v", err)
	}
	if a, _ := h.store.App("web"); a.WorkerCount("worker") != 3 || a.WorkerCount("clock") != 0 {
		t.Fatalf("scale %v", a.Scale)
	}

	// A crashed worker is restarted on its own.
	h.rt.Crash(cur.Workers[1].Container)
	if err := h.d.RecoverWorker(ctx, "web", "worker.2"); err != nil {
		t.Fatal(err)
	}
	after := h.store.CurrentRelease("web")
	if after.Workers[1].Container == cur.Workers[1].Container {
		t.Fatal("worker.2 wasn't replaced")
	}
	running(t, h, after.Workers[1].Container)

	// Stop takes the workers down; start brings them back at scale.
	if err := h.d.Stop(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	gone(t, h, after.Workers[0].Container)
	if err := h.d.Start(ctx, "web", &out); err != nil {
		t.Fatal(err)
	}
	if n := len(h.store.CurrentRelease("web").Workers); n != 3 {
		t.Fatalf("%d workers after start", n)
	}

	// Rolling back runs the old release's workers at today's scale.
	if _, err := h.d.Rollback(ctx, "web", 2, false, &out); err != nil {
		t.Fatal(err)
	}
	if n := len(h.store.CurrentRelease("web").Workers); n != 3 {
		t.Fatalf("%d workers after rollback", n)
	}
}

func TestBadCronFailsTheDeploy(t *testing.T) {
	h := newHarness(t)
	h.commit(map[string]string{"app.json": `{"cron":[{"command":"x","schedule":"daily"}]}`})
	var out bytes.Buffer
	if _, err := h.d.Deploy(context.Background(), Request{App: "web", Trigger: "test", Out: &out}); err == nil || !strings.Contains(err.Error(), "cron entry 1") {
		t.Fatalf("deploy with a bad schedule: %v", err)
	}
}

func TestRunJob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.commit(map[string]string{"app.json": `{"cron":[{"command":"node digest.js","schedule":"@daily"}]}`})
	rel, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var got runtime.RunOpts
	h.rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error {
		got = o
		io.WriteString(stdout, "sent 3 digests\n")
		return nil
	}
	var out bytes.Buffer
	if err := h.d.RunJob(ctx, "web", rel, 1, &out); err != nil {
		t.Fatal(err)
	}
	if got.Labels["dev.berth.role"] != "cron" || !got.Remove || got.Entrypoint != "/bin/sh" || strings.Join(got.Args, " ") != "-c node digest.js" || got.Image != rel.Image {
		t.Fatalf("ran %+v", got)
	}
	if out.String() != "sent 3 digests\n" {
		t.Fatalf("output %q", out.String())
	}
	if err := h.d.RunJob(ctx, "web", rel, 2, &out); err == nil {
		t.Fatal("job 2 of 1 ran")
	}
}
