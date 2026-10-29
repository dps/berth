package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/caddy"
	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

func newTestDaemon(t *testing.T) (*Daemon, *runtime.Fake, string) {
	t.Helper()
	os.Setenv("BERTH_NO_MDNS", "1")
	workerSettle = 0
	s := &state.Store{Root: t.TempDir()}
	cfg := state.DefaultConfig()
	cfg.PortMin, cfg.PortMax = 18300, 18399
	cfg.Domain = "example.com"
	s.SaveConfig(cfg)

	repo := t.TempDir()
	git := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(admin.Close)
	rt := runtime.NewFake()
	d := New(s, rt)
	d.Caddy = caddy.NewClient(strings.TrimPrefix(admin.URL, "http://"))
	d.Deployer.HealthInterval = 20 * time.Millisecond
	d.Deployer.MinFreeBytes = 0
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	return d, rt, repo
}

func (d *Daemon) waitIdle(t *testing.T, app string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 200; i++ {
		if !d.Deployer.Busy(app) && !d.isQueued(app) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("deploy did not finish")
}

func push(t *testing.T, h http.Handler, app, secret, delivery string, ev map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(ev)
	req := httptest.NewRequest("POST", "/github/"+app, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", github.Sign(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookDeploysOnceAndDedupes(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	out := &bytes.Buffer{}
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo, NoDeploy: true})
	if _, err := d.hCreate(d.ctx, params, out); err != nil {
		t.Fatal(err)
	}
	app, _ := d.Store.App("web")
	h := d.webhookHandler()
	ev := map[string]any{"ref": "refs/heads/main", "after": "abc1234def"}

	if rec := push(t, h, "web", "wrong-secret", "d0", ev); rec.Code != 401 {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	if rec := push(t, h, "web", app.Webhook.Secret, "d1", map[string]any{"ref": "refs/heads/other"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("other branch: %d %s", rec.Code, rec.Body)
	}
	if rec := push(t, h, "web", app.Webhook.Secret, "d2", map[string]any{"ref": "refs/tags/v1"}); !strings.Contains(rec.Body.String(), "not a branch") {
		t.Fatalf("tag push: %s", rec.Body)
	}
	if len(rt.CallLog("build")) != 0 {
		t.Fatal("ignored pushes built something")
	}
	rec := push(t, h, "web", app.Webhook.Secret, "d3", ev)
	if rec.Code != 202 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	d.waitIdle(t, "web")
	if n := len(rt.CallLog("build")); n != 1 {
		t.Fatalf("builds after push: %d", n)
	}
	// GitHub redelivery of the same delivery id is a no-op.
	rec = push(t, h, "web", app.Webhook.Secret, "d3", ev)
	if !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("redelivery: %s", rec.Body)
	}
	d.waitIdle(t, "web")
	if n := len(rt.CallLog("build")); n != 1 {
		t.Fatalf("redelivery rebuilt: %d builds", n)
	}
	if d.Store.Current("web") != 1 {
		t.Fatal("no current release")
	}
	// Unknown app and oversized bodies are rejected.
	if rec := push(t, h, "nope", "x", "d4", ev); rec.Code != 401 {
		t.Fatalf("unknown app should look like a bad signature: %d", rec.Code)
	}
	big := httptest.NewRequest("POST", "/github/web", bytes.NewReader(make([]byte, maxHookBody+10)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, big)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %d", rr.Code)
	}
}

func TestMonorepoWebhookPathFilter(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	params, _ := json.Marshal(CreateParams{Name: "api", Repo: repo, Path: "services/api", NoDeploy: true})
	d.hCreate(d.ctx, params, &bytes.Buffer{})
	app, _ := d.Store.App("api")
	ev := map[string]any{"ref": "refs/heads/main", "after": "1111111", "commits": []any{map[string]any{"modified": []string{"web/index.html"}}}}
	rec := push(t, d.webhookHandler(), "api", app.Webhook.Secret, "x1", ev)
	if !strings.Contains(rec.Body.String(), "no changes under services/api") {
		t.Fatalf("%s", rec.Body)
	}
	if len(rt.CallLog("build")) != 0 {
		t.Fatal("built for an unrelated path")
	}
}

func TestSupervisorRestartsCrashedApp(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, params, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cur := d.Store.CurrentRelease("web")
	d.supervise(d.ctx) // first sight: healthy
	rt.Crash(cur.Container)
	d.supervise(d.ctx)
	after := d.Store.CurrentRelease("web")
	if after.Container == cur.Container || after.Status != state.StatusRunning {
		t.Fatalf("not restarted: %+v", after)
	}
	evs, _ := os.ReadFile(d.Store.EventsPath())
	if !strings.Contains(string(evs), `"kind":"crash"`) {
		t.Fatal("crash event not logged")
	}
}

func TestStatusAndConfigSetRelease(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, params, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cp, _ := json.Marshal(ConfigParams{App: "web", Set: map[string]string{"FOO": "bar"}})
	res, err := d.hConfigSet(d.ctx, cp, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if rr := res.(*ReleaseResult); rr.Release != 2 {
		t.Fatalf("config:set made release %d", rr.Release)
	}
	if n := len(rt.CallLog("build")); n != 1 {
		t.Fatalf("config:set rebuilt (%d builds)", n)
	}
	st, err := d.Status(d.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Apps) != 1 || st.Apps[0].State != "running" || st.Apps[0].URLs[0] != "https://web.example.com" {
		t.Fatalf("%+v", st.Apps)
	}
}

func TestBackupArchiveExcludes(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"config.json", "apps/web/app.json", "apps/web/env", "apps/web/repo.git/HEAD", "apps/web/logs/app.log", "backups/pg/db/1.dump", "data/apps/web/data/x.db"} {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o700)
		os.WriteFile(full, []byte("x"), 0o600)
	}
	dest := filepath.Join(t.TempDir(), "b.tar.gz")
	n, err := WriteArchive(root, dest)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("archived %d files, want 5", n)
	}
	out := t.TempDir()
	if err := extractArchive(dest, out); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"apps/web/app.json", "backups/pg/db/1.dump", "data/apps/web/data/x.db", ".config.restored.json"} {
		if _, err := os.Stat(filepath.Join(out, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	for _, p := range []string{"apps/web/repo.git", "apps/web/logs"} {
		if _, err := os.Stat(filepath.Join(out, p)); err == nil {
			t.Errorf("%s should be excluded", p)
		}
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"512M": 512 << 20, "1G": 1 << 30, "1gb": 1 << 30, "256": 256 << 20, "": 0} {
		if got := ParseMemory(in); got != want {
			t.Errorf("%q = %d want %d", in, got, want)
		}
	}
}

// An app that crashes every time it is restarted is given up on after 10
// crashes, even though each restart itself succeeds; a restart resets it.
func TestCrashLoopGivesUp(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	d.CrashBackoff = func(int) time.Duration { return 0 }
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, params, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	d.supervise(d.ctx)
	restarts := 0
	for i := 0; i < 15; i++ {
		rt.Crash(d.Store.CurrentRelease("web").Container)
		before := len(rt.CallLog("run "))
		d.supervise(d.ctx)
		if len(rt.CallLog("run ")) > before {
			restarts++
		}
	}
	if restarts != maxCrashFailures {
		t.Fatalf("restarted %d times, want %d", restarts, maxCrashFailures)
	}
	if rel := d.Store.CurrentRelease("web"); rel.Status != state.StatusCrashed || rel.Container != "" {
		t.Fatalf("release after giving up: %+v", rel)
	}
	if cj, _ := os.ReadFile(d.Store.Path("caddy.json")); !strings.Contains(string(cj), "web is not running") {
		t.Fatal("a crashed app should get Caddy's immediate 503, not retries against dead slots")
	}
	evs, _ := os.ReadFile(d.Store.EventsPath())
	if strings.Count(string(evs), `"kind":"crash.loop"`) != 1 {
		t.Fatal("want exactly one crash.loop event")
	}
	if _, err := d.hRestart(d.ctx, json.RawMessage(`{"app":"web"}`), &bytes.Buffer{}); err != nil {
		t.Fatalf("restart after crash loop: %v", err)
	}
	if rel := d.Store.CurrentRelease("web"); rel.Status != state.StatusRunning || rel.Container == "" {
		t.Fatalf("not running after restart: %+v", rel)
	}
}

func TestNetworkLossIsHealed(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, params, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	rt.RunWithErr = errors.New("wget: download timed out")
	d.checkNetwork(d.ctx) // first failure: wait and see
	if len(rt.CallLog("system stop")) != 0 {
		t.Fatal("healed after a single failed probe")
	}
	d.net.lastProbe = time.Time{}
	d.checkNetwork(d.ctx) // second failure: heal
	if len(rt.CallLog("system stop")) != 1 {
		t.Fatal("did not restart the container system after two failed probes")
	}
	cur := d.Store.CurrentRelease("web")
	cs, _ := rt.List(d.ctx)
	if c, ok := runtime.Find(cs, cur.Container); !ok || !c.Running() {
		t.Fatalf("app not running after heal: %+v", cur)
	}
	if st := d.crash["web"]; st != nil && st.failures > 0 {
		t.Fatal("a deliberate container-system restart counted as a crash")
	}
	off := false
	cfg, _ := d.Store.Config()
	cfg.AutoHealNetwork = &off
	d.Store.SaveConfig(cfg)
	for i := 0; i < 3; i++ {
		d.net.lastProbe = time.Time{}
		d.checkNetwork(d.ctx)
	}
	if len(rt.CallLog("system stop")) != 1 {
		t.Fatal("healed with auto_heal_network off")
	}
}

// Junk requests from one IP are throttled without touching the app's budget
// for real, signed deliveries.
func TestWebhookFailuresDontStarveRealDeliveries(t *testing.T) {
	d, _, repo := newTestDaemon(t)
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo, NoDeploy: true})
	d.hCreate(d.ctx, params, &bytes.Buffer{})
	app, _ := d.Store.App("web")
	h := d.webhookHandler()
	ev := map[string]any{"ref": "refs/heads/other"}
	for i := 0; i < 50; i++ {
		push(t, h, "web", "wrong", fmt.Sprintf("junk-%d", i), ev)
	}
	if rec := push(t, h, "web", "wrong", "junk-x", ev); rec.Code != 429 {
		t.Fatalf("attacker IP not throttled: %d", rec.Code)
	}
	// GitHub comes from another address.
	body, _ := json.Marshal(ev)
	req := httptest.NewRequest("POST", "/github/web", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "real-1")
	req.Header.Set("X-Hub-Signature-256", github.Sign(app.Webhook.Secret, body))
	req.Header.Set("Cf-Connecting-Ip", "140.82.115.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("real delivery blocked: %d %s", rec.Code, rec.Body)
	}
}
