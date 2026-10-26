package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dps/berth/internal/state"
)

// TestMoveToANewMac backs up one Berth and brings it up on another the way
// `berth init --restore` does: secrets and settings first, then restore.
func TestMoveToANewMac(t *testing.T) {
	old, _, repo := newTestDaemon(t)
	cfg := old.cfg()
	cfg.Domain, cfg.TunnelID, cfg.Hostname = "old.example", "tunnel-1", "oldmac"
	old.Store.SaveConfig(cfg)
	os.MkdirAll(old.Store.SecretsDir(), 0o700)
	os.WriteFile(filepath.Join(old.Store.SecretsDir(), "github.token"), []byte("ghtok\n"), 0o600)
	cp, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := old.hCreate(old.ctx, cp, io.Discard); err != nil {
		t.Fatal(err)
	}
	old.updateApp("web", func(a *state.App) {
		a.Access = &state.Access{Level: state.LevelAllow, Allow: []string{"bob@example.com"}}
		a.Domains = []string{"www.mysite.com"}
	})
	old.Store.SaveEnv("web", state.Env{"GREETING": "hi"})
	old.Store.SaveApp(&state.App{Name: "web-dev", DevOf: "web", Branch: devRef})
	old.Store.SaveApp(&state.App{Name: "web-pr-4", DevOf: "web", Preview: 4, Branch: previewRef(4)})
	live := old.Store.CurrentRelease("web").SHA
	// The branch moves on after the last deploy.
	commitFiles(t, repo, map[string]string{"later.txt": "not deployed"})

	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := WriteArchive(old.Store.Root, archive); err != nil {
		t.Fatal(err)
	}

	// The new Mac: its own hostname and gateway.
	nu, rt, _ := newTestDaemon(t)
	ncfg := nu.cfg()
	ncfg.Hostname, ncfg.Domain = "newmac", ""
	nu.Store.SaveConfig(ncfg)
	if err := SeedFromArchive(archive, nu.Store); err != nil {
		t.Fatal(err)
	}
	seeded := nu.cfg()
	if seeded.Domain != "old.example" || seeded.TunnelID != "tunnel-1" || seeded.Hostname != "newmac" {
		t.Fatalf("seeded config %+v", seeded)
	}
	if b, _ := os.ReadFile(filepath.Join(nu.Store.SecretsDir(), "github.token")); string(b) != "ghtok\n" {
		t.Fatal("secrets not seeded")
	}
	if nu.Store.AppExists("web") {
		t.Fatal("seeding brought back apps; that's restore's job")
	}

	var out bytes.Buffer
	rp, _ := json.Marshal(map[string]string{"archive": archive})
	if _, err := nu.hRestore(nu.ctx, rp, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	cur := nu.Store.CurrentRelease("web")
	if cur == nil || cur.SHA != live || cur.Status != state.StatusRunning {
		t.Fatalf("restored release %+v, want %s live\n%s", cur, live, out.String())
	}
	a, _ := nu.Store.App("web")
	env, _ := nu.Store.Env("web")
	if a.Access == nil || a.Access.Allow[0] != "bob@example.com" || a.Domains[0] != "www.mysite.com" || env["GREETING"] != "hi" {
		t.Fatalf("app settings %+v env %v", a, env)
	}
	if nu.Store.AppExists("web-pr-4") || !nu.Store.AppExists("web-dev") {
		t.Fatal("previews should go (their pull requests bring them back); dev versions stay")
	}
	if !strings.Contains(out.String(), "web-dev is a dev version") || !strings.Contains(out.String(), ".newmac.local") {
		t.Fatalf("restore output:\n%s", out.String())
	}
	if len(rt.CallLog("build")) != 1 {
		t.Fatalf("builds %v", rt.CallLog("build"))
	}
	nu.stopTails()
	old.stopTails()
}
