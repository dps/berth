package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// snapshotInto commits repo's working tree on top of HEAD (as dev:push
// does) and pushes it to app's mirror at refs/berth/dev.
func snapshotInto(t *testing.T, d *Daemon, repo, app string) string {
	t.Helper()
	run := func(env []string, args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"), env...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	idx := filepath.Join(t.TempDir(), "index")
	run([]string{"GIT_INDEX_FILE=" + idx}, "add", "-A")
	tree := run([]string{"GIT_INDEX_FILE=" + idx}, "write-tree")
	sha := run(nil, "commit-tree", tree, "-p", "HEAD", "-m", "snapshot")
	run(nil, "push", "-q", "--force", d.Store.Path("apps", app, "repo.git"), sha+":refs/berth/dev")
	return sha
}

func TestDevVersionLifecycle(t *testing.T) {
	d, _, repo := newTestDaemon(t)
	ctx := context.Background()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	d.auth = auth.NewServer(auth.OpenSessions(filepath.Join(t.TempDir(), "s.json")), key)
	d.Store.SaveAuthConfig(&state.AuthConfig{Provider: "google", ClientID: "x.apps.googleusercontent.com", Host: "auth.example.com"})
	d.configureAuth()

	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"pg", "redis"} {
		if _, err := d.Services.Create(ctx, kind, "data", "", "", io.Discard); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(SvcParams{Kind: kind, Name: "data", App: "web", NoRestart: true})
		if _, err := d.hSvcLink(ctx, b, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	env, _ := d.Store.Env("web")
	env["OPENAI_API_KEY"] = "sk-test"
	d.Store.SaveEnv("web", env)
	d.Apply(ctx)
	conf, _ := os.ReadFile(d.Store.Path("caddy.json"))
	if !strings.Contains(string(conf), "web has no dev version yet") {
		t.Fatal("no placeholder for web-dev before the first push")
	}
	// Names that would collide with dev addresses are refused.
	b, _ = json.Marshal(CreateParams{Name: "web-dev", Repo: repo, NoDeploy: true})
	if _, err := d.hCreate(ctx, b, io.Discard); err == nil {
		t.Fatal("created an app on web's dev address")
	}

	os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("uncommitted"), 0o644)
	sha := snapshotInto(t, d, repo, "web")
	b, _ = json.Marshal(DevDeployParams{App: "web", Ref: sha, Base: "abc", Changes: "1 file changed"})
	out, err := d.hDevDeploy(ctx, b, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(*DevDeployResult)
	if res.Status != "live" || res.Dev != "web-dev" || res.SHA != sha || !res.Dirty || res.URL != "https://web-dev.example.com" {
		t.Fatalf("result %+v", res)
	}
	dev, _ := d.Store.App("web-dev")
	if dev.DevOf != "web" || dev.Access == nil || dev.Access.Allow[0] != "@owners" {
		t.Fatalf("dev app %+v", dev)
	}
	devEnv, _ := d.Store.Env("web-dev")
	if devEnv["OPENAI_API_KEY"] != "sk-test" || devEnv["BERTH_KEEP_RELEASES"] != "3" {
		t.Fatalf("dev env %v", devEnv)
	}
	if !strings.HasSuffix(devEnv["DATABASE_URL"], "/data_dev") || devEnv["DATABASE_URL"] == env["DATABASE_URL"] || !strings.HasPrefix(devEnv["DATABASE_URL"], "postgres://web_dev:") {
		t.Fatalf("dev DATABASE_URL %q", devEnv["DATABASE_URL"])
	}
	devRedis, err := d.Store.Service("redis", "data-dev")
	if err != nil || !strings.Contains(devEnv["REDIS_URL"], ":"+strconv.Itoa(devRedis.Port)) {
		t.Fatalf("dev redis %v %q", err, devEnv["REDIS_URL"])
	}
	conf, _ = os.ReadFile(d.Store.Path("caddy.json"))
	if strings.Contains(string(conf), "web has no dev version yet") || !strings.Contains(string(conf), "noindex") {
		t.Fatal("dev route not in caddy config")
	}

	// A token gets an agent past @owners, signed in as its pretend user.
	b, _ = json.Marshal(DevParams{App: "web", As: "test@example.com"})
	tr, err := d.hDevToken(ctx, b, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	tok := tr.(map[string]string)["token"]
	check := func(host, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/_berth/check", nil)
		r.Host = host
		r.Header.Set("X-Forwarded-Uri", "/api/me")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		d.auth.ServeHTTP(w, r)
		return w
	}
	if w := check("web-dev.example.com", tok); w.Code != 200 || w.Header().Get("X-Berth-User-Id") != "dev:test@example.com" {
		t.Fatalf("token: %d %v", w.Code, w.Header())
	}
	if w := check("web-dev.example.com", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := check("web-dev.example.com", auth.NewToken()); w.Code != http.StatusUnauthorized {
		t.Fatalf("made-up token: %d", w.Code)
	}

	// Idle dev versions sleep, and the next push wakes them.
	rel := d.Store.CurrentRelease("web-dev")
	rel.StartedAt = time.Now().Add(-13 * time.Hour)
	d.Store.SaveRelease("web-dev", rel)
	d.sleepIdleDev(ctx)
	if a, _ := d.Store.App("web-dev"); !a.Stopped {
		t.Fatal("idle dev version still running")
	}
	b, _ = json.Marshal(DevDeployParams{App: "web", Ref: sha})
	if out, _ := d.hDevDeploy(ctx, b, io.Discard); out.(*DevDeployResult).Status != "live" {
		t.Fatal("push didn't wake the dev version")
	}
	if a, _ := d.Store.App("web-dev"); a.Stopped {
		t.Fatal("still asleep after a push")
	}
	// Production was never touched.
	if r := d.Store.CurrentRelease("web"); r.N != 1 {
		t.Fatalf("production moved to v%d", r.N)
	}

	b, _ = json.Marshal(DevParams{App: "web"})
	if _, err := d.hDevReset(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	if d.Store.AppExists("web-dev") {
		t.Fatal("dev version survived reset")
	}
	if _, err := d.Store.Service("redis", "data-dev"); err == nil {
		t.Fatal("dev redis survived reset")
	}
	if sv, _ := d.Store.Service("pg", "data"); sv.Links["web-dev"].Role != "" || sv.Links["web"].Role == "" {
		t.Fatalf("pg links after reset: %v", sv.Links)
	}

	// dev:off removes the address; destroying the parent takes the dev version too.
	b, _ = json.Marshal(DevParams{App: "web", Off: true})
	d.hDevSet(ctx, b, io.Discard)
	conf, _ = os.ReadFile(d.Store.Path("caddy.json"))
	if strings.Contains(string(conf), "web-dev.example.com") {
		t.Fatal("dev address still routed after dev:off")
	}
	b, _ = json.Marshal(DevDeployParams{App: "web", Ref: sha})
	if _, err := d.hDevDeploy(ctx, b, io.Discard); err == nil {
		t.Fatal("dev:push worked with dev versions off")
	}
	b, _ = json.Marshal(DevParams{App: "web"})
	d.hDevSet(ctx, b, io.Discard)
	b, _ = json.Marshal(DevDeployParams{App: "web", Ref: sha})
	d.hDevDeploy(ctx, b, io.Discard)
	b, _ = json.Marshal(AppParams{App: "web"})
	if _, err := d.hDestroy(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	if d.Store.AppExists("web-dev") {
		t.Fatal("dev version outlived its app")
	}
}

func TestDevBuildFailureIsReported(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	ctx := context.Background()
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	sha := snapshotInto(t, d, repo, "web")
	for behaviour, stage := range map[string]string{"build error": "build", "unhealthy": "start"} {
		rt.BuildHook = func(o runtime.BuildOpts) (string, error) {
			if behaviour == "build error" {
				return "", fmt.Errorf("npm ERR! missing script: build")
			}
			return behaviour, nil
		}
		d.Store.SaveEnv("web-dev", state.Env{state.KeyHealthTimeout: "1"})
		b, _ = json.Marshal(DevDeployParams{App: "web", Ref: sha})
		out, err := d.hDevDeploy(ctx, b, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if res := out.(*DevDeployResult); res.Status != "failed" || res.Stage != stage {
			t.Fatalf("%s: result %+v", behaviour, res)
		}
	}
}
