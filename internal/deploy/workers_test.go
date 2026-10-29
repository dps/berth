package deploy

import (
	"bytes"
	"context"
	"fmt"
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

// hookCalls records each call of the Workers hook as "v<n> <container>",
// or "stopped" when the app has no live container.
func hookCalls(h *harness) *[]string {
	var calls []string
	h.d.Workers = func(ctx context.Context, app string, out io.Writer) {
		rel := h.store.CurrentRelease(app)
		a, _ := h.store.App(app)
		if a.Stopped || rel == nil || rel.Container == "" {
			calls = append(calls, "stopped")
			return
		}
		calls = append(calls, fmt.Sprintf("v%d %s", rel.N, rel.Container))
	}
	return &calls
}

func TestWorkersFollowTheRelease(t *testing.T) {
	h := newHarness(t)
	calls := hookCalls(h)
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
	if r1.Processes["worker"] != "node worker.js" || len(r1.Cron) != 1 || r1.Cron[0].Schedule != "0 7 * * *" {
		t.Fatalf("v1 processes %v cron %v", r1.Processes, r1.Cron)
	}
	last := func() string { return (*calls)[len(*calls)-1] }
	// Each change hands the live container to berthd once it's current.
	if len(*calls) != 1 || last() != "v1 "+r1.Container {
		t.Fatalf("after deploy: %v", *calls)
	}
	h.commit(map[string]string{"x.txt": "2"})
	r2, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test", Out: &out})
	if err != nil || last() != "v2 "+r2.Container {
		t.Fatalf("second deploy: %v %v", err, *calls)
	}
	// Config changes keep the Procfile's processes and the cron list.
	h.store.SaveEnv("web", state.Env{"BERTH_DRAIN": "0", "BERTH_HEALTH_TIMEOUT": "3", "A": "1"})
	r3, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "config", ReuseImage: true, Out: &out})
	if err != nil || len(r3.Processes) != 2 || len(r3.Cron) != 1 {
		t.Fatalf("config release: %v %+v", err, r3)
	}

	// Scale.
	n := len(*calls)
	if err := h.d.Scale(ctx, "web", "worker", 3, &out); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Scale(ctx, "web", "clock", 0, &out); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != n+2 {
		t.Fatalf("scale calls %v", *calls)
	}
	if err := h.d.Scale(ctx, "web", "nope", 1, &out); err == nil || !strings.Contains(err.Error(), "clock, worker") {
		t.Fatalf("unknown type: %v", err)
	}
	if a, _ := h.store.App("web"); a.WorkerCount("worker") != 3 || a.WorkerCount("clock") != 0 {
		t.Fatalf("scale %v", a.Scale)
	}

	// Stop, start, restart and rollback each tell berthd.
	if err := h.d.Stop(ctx, "web"); err != nil || last() != "stopped" {
		t.Fatalf("stop: %v %v", err, *calls)
	}
	if err := h.d.Start(ctx, "web", &out); err != nil || last() != "v3 "+h.store.CurrentRelease("web").Container {
		t.Fatalf("start: %v %v", err, *calls)
	}
	if err := h.d.Restart(ctx, "web", &out); err != nil || last() != "v3 "+h.store.CurrentRelease("web").Container {
		t.Fatalf("restart: %v %v", err, *calls)
	}
	if _, err := h.d.Rollback(ctx, "web", 2, false, &out); err != nil || last() != fmt.Sprintf("v%d %s", h.store.CurrentRelease("web").N, h.store.CurrentRelease("web").Container) {
		t.Fatalf("rollback: %v %v", err, *calls)
	}
	// No worker ever got a container of its own.
	for _, c := range h.rt.CallLog("run ") {
		if strings.Contains(c, "worker") || strings.Contains(c, "clock") {
			t.Fatalf("a worker container ran: %s", c)
		}
	}
}

func TestOldWorkerContainersAreRemoved(t *testing.T) {
	h := newHarness(t)
	hookCalls(h)
	ctx := context.Background()
	h.commit(map[string]string{"Procfile": "worker: node worker.js\n"})
	r1, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// As an older Berth left it: a worker in its own container.
	h.rt.Run(ctx, runtime.RunOpts{Name: "berth-web-1-worker1-x", Image: r1.Image, Detach: true})
	r1.Workers = []state.Worker{{Type: "worker", Index: 1, Container: "berth-web-1-worker1-x"}}
	h.store.SaveRelease("web", r1)
	h.commit(map[string]string{"x.txt": "2"})
	if _, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test"}); err != nil {
		t.Fatal(err)
	}
	h.waitRetired(t, r1.Container)
	cs, _ := h.rt.List(ctx)
	if _, ok := runtime.Find(cs, "berth-web-1-worker1-x"); ok {
		t.Fatal("the old worker container is still there")
	}
	if old, _ := h.store.Release("web", 1); len(old.Workers) != 0 {
		t.Fatalf("v1 still lists workers %+v", old.Workers)
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
