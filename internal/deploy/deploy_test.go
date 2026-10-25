package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

type fakeRouter struct {
	mu     sync.Mutex
	calls  int
	store  *state.Store
	events []string // "apply cur=<n>"
}

func (r *fakeRouter) Apply(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.events = append(r.events, fmt.Sprintf("apply cur=%d", r.store.Current("web")))
	return nil
}

func gitRepo(t *testing.T, files map[string]string) (string, func(map[string]string) string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	commit := func(fs map[string]string) string {
		for name, body := range fs {
			p := filepath.Join(dir, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(body), 0o644)
		}
		run("add", "-A")
		run("commit", "-q", "-m", "change")
		return run("rev-parse", "HEAD")
	}
	commit(files)
	return dir, commit
}

type harness struct {
	store  *state.Store
	rt     *runtime.Fake
	router *fakeRouter
	d      *Deployer
	repo   string
	commit func(map[string]string) string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := &state.Store{Root: t.TempDir()}
	cfg := state.DefaultConfig()
	cfg.PortMin, cfg.PortMax = 18100, 18199
	cfg.KeepReleases = 2
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	repo, commit := gitRepo(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	if err := s.SaveApp(&state.App{Name: "web", Repo: repo, Branch: "main", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.SaveEnv("web", state.Env{"BERTH_DRAIN": "0", "BERTH_HEALTH_TIMEOUT": "3"})
	rt := runtime.NewFake()
	router := &fakeRouter{store: s}
	d := New(s, rt, router)
	d.HealthInterval = 20 * time.Millisecond
	d.MinFreeBytes = 0
	return &harness{store: s, rt: rt, router: router, d: d, repo: repo, commit: commit}
}

func (h *harness) waitRetired(t *testing.T, container string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		cs, _ := h.rt.List(context.Background())
		if _, ok := runtime.Find(cs, container); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was not retired", container)
}

func get(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestDeployOrdering(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var out bytes.Buffer
	r1, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test", Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if r1.N != 1 || r1.Status != state.StatusRunning || h.store.Current("web") != 1 {
		t.Fatalf("release 1: %+v", r1)
	}
	if !strings.Contains(get(t, r1.HostPort), r1.Container) {
		t.Fatal("v1 not serving")
	}
	// PORT is injected; BERTH_* settings are not passed to the app.
	if r1.Env["PORT"] != "8080" || r1.Env["BERTH_DRAIN"] != "" {
		t.Fatalf("env %v", r1.Env)
	}

	h.commit(map[string]string{"x.txt": "2"})
	r2, err := h.d.Deploy(ctx, Request{App: "web", Trigger: "test", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if r2.HostPort == r1.HostPort {
		t.Fatal("v2 must run side by side on a new port")
	}
	// Ordering: build v2 → run v2 → (health) → route switch → stop v1.
	calls := h.rt.CallLog("")
	idx := func(prefix string) int {
		for i, c := range calls {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	if !(idx("build berth/web:2") < idx("run "+r2.Container)) {
		t.Fatalf("build must precede run: %v", calls)
	}
	h.waitRetired(t, r1.Container)
	calls = h.rt.CallLog("")
	if idx("stop "+r1.Container) < idx("run "+r2.Container) {
		t.Fatalf("old container stopped before new one ran: %v", calls)
	}
	if last := h.router.events[len(h.router.events)-1]; last != "apply cur=2" {
		t.Fatalf("router last saw %s", last)
	}
	var old *state.Release
	for i := 0; i < 50; i++ {
		if old, _ = h.store.Release("web", 1); old.Container == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if old.Status != state.StatusStopped || old.Container != "" {
		t.Fatalf("v1 after swap: %+v", old)
	}
}

func TestFailedHealthKeepsOldRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.BuildHook = func(o runtime.BuildOpts) (string, error) { return "unhealthy", nil }
	h.commit(map[string]string{"x": "bad"})
	var out bytes.Buffer
	r2, err := h.d.Deploy(ctx, Request{App: "web", Out: &out})
	if err == nil || !strings.Contains(err.Error(), "health check") {
		t.Fatalf("want health failure, got %v", err)
	}
	if r2.Status != state.StatusFailed {
		t.Fatalf("v2 status %s", r2.Status)
	}
	if h.store.Current("web") != 1 {
		t.Fatal("current moved off v1")
	}
	if !strings.Contains(get(t, r1.HostPort), r1.Container) {
		t.Fatal("v1 stopped serving")
	}
	cs, _ := h.rt.List(ctx)
	if _, ok := runtime.Find(cs, r2.Container); ok {
		t.Fatal("failed container left running")
	}
	if !strings.Contains(out.String(), "still serving") {
		t.Errorf("output should say v1 is still serving:\n%s", out.String())
	}
}

func TestBuildFailure(t *testing.T) {
	h := newHarness(t)
	h.rt.BuildHook = func(o runtime.BuildOpts) (string, error) { return "", fmt.Errorf("build failed: boom") }
	rel, err := h.d.Deploy(context.Background(), Request{App: "web", Out: io.Discard})
	if err == nil || rel.Status != state.StatusFailed || h.store.Current("web") != 0 {
		t.Fatalf("err=%v rel=%+v", err, rel)
	}
	if len(h.rt.CallLog("run")) != 0 {
		t.Fatal("ran a container after a failed build")
	}
}

func TestRollbackAndConfigRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r1, _ := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	h.commit(map[string]string{"x": "2"})
	r2, _ := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	h.waitRetired(t, r1.Container)

	// Config change: new release, same image, no build.
	env, _ := h.store.Env("web")
	env["GREETING"] = "hi"
	h.store.SaveEnv("web", env)
	builds := len(h.rt.CallLog("build"))
	r3, err := h.d.Deploy(ctx, Request{App: "web", ReuseImage: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rt.CallLog("build")) != builds || r3.Image != r2.Image || r3.Env["GREETING"] != "hi" {
		t.Fatalf("config release rebuilt or lost env: %+v", r3)
	}
	h.waitRetired(t, r2.Container)

	rb, err := h.d.Rollback(ctx, "web", 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if rb.N != 2 || h.store.Current("web") != 2 {
		t.Fatalf("rolled back to v%d, current %d", rb.N, h.store.Current("web"))
	}
	if rb.Env["GREETING"] != "" {
		t.Fatal("rollback must use the release's own config snapshot")
	}
	if _, err := h.d.Rollback(ctx, "web", 2, io.Discard); err == nil {
		t.Fatal("rollback to current should fail")
	}
}

func TestGCKeepsWindow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var last *state.Release
	for i := 0; i < 4; i++ {
		h.commit(map[string]string{"n": fmt.Sprint(i)})
		r, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		if last != nil {
			h.waitRetired(t, last.Container)
		}
		last = r
	}
	h.d.GC(ctx, "web", io.Discard)
	rs, _ := h.store.Releases("web")
	var pruned []int
	for _, r := range rs {
		if r.Pruned {
			pruned = append(pruned, r.N)
		}
	}
	if fmt.Sprint(pruned) != "[2 1]" {
		t.Fatalf("pruned %v, want [2 1] (keep=2)", pruned)
	}
	if len(h.rt.CallLog("rmi berth/web:1")) != 1 {
		t.Fatalf("image 1 not removed: %v", h.rt.CallLog("rmi"))
	}
	if _, err := h.d.Rollback(ctx, "web", 1, io.Discard); err == nil || !strings.Contains(err.Error(), "garbage-collected") {
		t.Fatalf("rollback to pruned: %v", err)
	}
}

func TestRecoverAfterCrash(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r1, _ := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	h.rt.Crash(r1.Container)
	if err := h.d.Recover(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	cur := h.store.CurrentRelease("web")
	if cur.Container == r1.Container || cur.Status != state.StatusRunning {
		t.Fatalf("not recovered: %+v", cur)
	}
	if !strings.Contains(get(t, cur.HostPort), cur.Container) {
		t.Fatal("recovered container not serving")
	}
}

func TestStopStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err := h.d.Stop(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	a, _ := h.store.App("web")
	if !a.Stopped || h.store.CurrentRelease("web").Container != "" {
		t.Fatal("not stopped")
	}
	if err := h.d.Start(ctx, "web", io.Discard); err != nil {
		t.Fatal(err)
	}
	a, _ = h.store.App("web")
	if a.Stopped || h.store.CurrentRelease("web").Container == "" {
		t.Fatal("not started")
	}
}

func TestMonorepoPathAndDockerfileOverride(t *testing.T) {
	h := newHarness(t)
	h.commit(map[string]string{"services/api/deploy/Dockerfile": "FROM scratch\n"})
	h.store.SaveApp(&state.App{Name: "web", Repo: h.repo, Branch: "main", Path: "services/api"})
	env, _ := h.store.Env("web")
	env["BERTH_DOCKERFILE"] = "deploy/Dockerfile"
	h.store.SaveEnv("web", env)
	var out bytes.Buffer
	if _, err := h.d.Deploy(context.Background(), Request{App: "web", Out: &out}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "using deploy/Dockerfile") {
		t.Fatalf("override not used:\n%s", out.String())
	}
}
