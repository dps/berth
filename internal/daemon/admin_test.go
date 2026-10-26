package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/analytics"
	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
)

func TestAdminPage(t *testing.T) {
	d, _, repo := newTestDaemon(t)
	ctx := context.Background()
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	env, _ := d.Store.Env("web")
	env["SECRET_KEY"] = "hunter2"
	d.Store.SaveEnv("web", env)

	// Off until sign-in is set up and admin:enable runs.
	b, _ = json.Marshal(AdminParams{Owner: "me@example.com"})
	if _, err := d.hAdminEnable(ctx, b, io.Discard); err == nil || !strings.Contains(err.Error(), "auth:setup") {
		t.Fatalf("enabled without sign-in: %v", err)
	}
	d.Store.SaveAuthConfig(&state.AuthConfig{Provider: "google", ClientID: "id", Host: "auth.example.com"})
	if _, err := d.hAdminEnable(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	ac, _ := d.Store.AuthConfig()
	if ac.AdminHost != "admin.example.com" || strings.Join(ac.Groups["owners"], ",") != "me@example.com" {
		t.Fatalf("auth config %+v", ac)
	}
	// Caddy sends the admin host through sign-in to berthd's admin server.
	conf, _ := os.ReadFile(d.Store.Path("caddy.json"))
	if !strings.Contains(string(conf), `"admin.example.com"`) || !strings.Contains(string(conf), d.cfg().AdminAddr) || !strings.Contains(string(conf), "/_berth/check") {
		t.Fatalf("no admin route through sign-in in caddy.json")
	}
	// A hostname clash is refused.
	b, _ = json.Marshal(AdminParams{Host: "web.example.com"})
	if _, err := d.hAdminEnable(ctx, b, io.Discard); err == nil {
		t.Fatal("admin page took an app's hostname")
	}

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	d.authKey = key
	d.stats = analytics.NewStats()
	now := time.Now()
	logLine := fmt.Sprintf(`{"ts":%f,"duration":0.012,"size":100,"status":200,"request":{"client_ip":"1.2.3.4","method":"GET","host":"web.example.com","uri":"/hello","headers":{"User-Agent":["Mozilla/5.0 Chrome/140.0 Safari/537.36"]}}}`, float64(now.Unix()))
	r, _ := analytics.Parse([]byte(logLine))
	d.stats.Add("web", r)

	h := d.adminHandler()
	get := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if token != "" {
			req.Header.Set("X-Berth-Identity", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	id := auth.Identity{Provider: "google", Subject: "1", Email: "me@example.com", Name: "Me"}
	good := auth.IdentityToken(key, "auth.example.com", "admin.example.com", id, now)
	otherHost := auth.IdentityToken(key, "auth.example.com", "web.example.com", id, now)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	forged := auth.IdentityToken(otherKey, "auth.example.com", "admin.example.com", id, now)
	for name, tok := range map[string]string{"none": "", "another app's": otherHost, "forged": forged} {
		if w := get("/api/overview", tok); w.Code != 403 {
			t.Fatalf("%s identity: %d", name, w.Code)
		}
	}

	w := get("/api/overview", good)
	var ov Overview
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ov) != nil {
		t.Fatalf("overview %d %s", w.Code, w.Body.String())
	}
	if ov.Viewer.Email != "me@example.com" || len(ov.Status.Apps) != 1 || ov.Summaries["web"].Totals.Req != 1 || len(ov.Summaries["web"].Spark) != 24 || ov.All.Totals.Req != 1 || ov.All.Totals.P50 <= 0 || len(ov.All.Series) != 24 {
		t.Fatalf("overview %+v", ov.Summaries)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP %q", csp)
	}

	w = get("/api/apps/web?range=1h", good)
	if w.Code != 200 || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("app detail %d (leaks env values: %v)", w.Code, strings.Contains(w.Body.String(), "hunter2"))
	}
	var det AppDetail
	json.Unmarshal(w.Body.Bytes(), &det)
	if det.Report.Totals.Req != 1 || len(det.Report.Series) != 60 || det.Report.Paths[0].Key != "/hello" || len(det.Releases) == 0 || !strings.Contains(strings.Join(det.Info.EnvKeys, ","), "SECRET_KEY") {
		t.Fatalf("detail %+v %+v", det.Report.Totals, det.Info)
	}
	if w := get("/api/apps/nope", good); w.Code != 404 {
		t.Fatalf("unknown app %d", w.Code)
	}
	if w := get("/api/apps/web/live?since=0", good); w.Code != 200 || !strings.Contains(w.Body.String(), `"/hello"`) {
		t.Fatalf("live %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/events", good); w.Code != 200 || !strings.Contains(w.Body.String(), `"create"`) && !strings.Contains(w.Body.String(), `"deploy.ok"`) {
		t.Fatalf("events %d %s", w.Code, w.Body.String())
	}
	// The page and its assets; client-side routes get the page.
	if w := get("/", good); w.Code != 200 || !strings.Contains(w.Body.String(), `src="/app.js"`) {
		t.Fatalf("index %d", w.Code)
	}
	if w := get("/app.js", good); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("app.js %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get("/icon.svg", good); w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("icon.svg %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get("/apps/web", good); w.Code != 200 || !strings.Contains(w.Body.String(), "<main") {
		t.Fatalf("client route %d", w.Code)
	}

	// Off again: the route goes, and the server answers nothing.
	b, _ = json.Marshal(AdminParams{Disable: true})
	d.hAdminEnable(ctx, b, io.Discard)
	if w := get("/api/overview", good); w.Code != 404 {
		t.Fatalf("after disable %d", w.Code)
	}
	if conf, _ := os.ReadFile(d.Store.Path("caddy.json")); strings.Contains(string(conf), "admin.example.com") {
		t.Fatal("admin route left in caddy.json")
	}
}
