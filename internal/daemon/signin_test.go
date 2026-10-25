package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

func TestAccessPolicyRoutesThroughSignIn(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	ctx := context.Background()
	d.Store.SaveApp(&state.App{Name: "chirp", Repo: "o/r", Branch: "main"})
	d.auth = auth.NewServer(auth.OpenSessions(d.Store.Path("auth", "sessions.json")), nil)

	call := func(method string, p any) error {
		b, _ := json.Marshal(p)
		h := map[string]func(context.Context, json.RawMessage, io.Writer) (any, error){
			"access": d.hAccessSet, "group": d.hAuthGroup, "setup": d.hAuthSetup,
		}[method]
		_, err := h(ctx, b, io.Discard)
		return err
	}
	for _, bad := range []AccessParams{
		{App: "chirp", Level: "everyone"},
		{App: "chirp", Level: "allow"},
		{App: "chirp", Level: "allow", Allow: []string{"not-an-email"}},
		{App: "chirp", Level: "public", Allow: []string{"a@b.co"}},
		{App: "chirp", Path: "admin", Level: "signed-in"},
		{App: "chirp", Path: "/a b", Level: "signed-in"},
	} {
		if err := call("access", bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := call("access", AccessParams{App: "chirp", Path: "/admin/*", Level: "allow", Allow: []string{"@family, Bob@Gmail.com"}}); err != nil {
		t.Fatal(err)
	}
	a, _ := d.Store.App("chirp")
	if a.Access == nil || a.Access.Level != "public" || len(a.Access.Paths) != 1 || strings.Join(a.Access.Paths[0].Allow, " ") != "@family bob@gmail.com" {
		t.Fatalf("access %+v", a.Access)
	}
	conf, _ := os.ReadFile(d.Store.Path("caddy.json"))
	if !strings.Contains(string(conf), `"/_berth/check"`) || !strings.Contains(string(conf), `"private, no-cache"`) {
		t.Fatal("caddy config has no forward_auth for chirp")
	}
	// No provider yet: the auth host isn't routed.
	if strings.Contains(string(conf), "auth.example.com") {
		t.Fatal("auth host routed before setup")
	}
	if err := call("group", AuthGroupParams{Name: "family", Members: []string{"@other"}}); err == nil {
		t.Fatal("group inside a group accepted")
	}
	if err := call("setup", AuthSetupParams{Provider: "google", ClientID: "nope", ClientSecret: "s"}); err == nil {
		t.Fatal("bad client id accepted")
	}
	if err := call("setup", AuthSetupParams{Provider: "google", ClientID: "1-abc.apps.googleusercontent.com", ClientSecret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	conf, _ = os.ReadFile(d.Store.Path("caddy.json"))
	if !strings.Contains(string(conf), `"auth.example.com"`) {
		t.Fatal("auth host not routed after setup")
	}
	// Sign-in reaches the provider for a known app host.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/_berth/login", nil)
	r.Host = "chirp.example.com"
	d.auth.ServeHTTP(w, r)
	if w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "https://auth.example.com/start?host=chirp.example.com") {
		t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	if err := call("access", AccessParams{App: "chirp", Off: true}); err != nil {
		t.Fatal(err)
	}
	conf, _ = os.ReadFile(d.Store.Path("caddy.json"))
	if strings.Contains(string(conf), `"/_berth/check"`) {
		t.Fatal("forward_auth left after access off")
	}
}

func TestSignInHostRefusesNamesWithTheirOwnDNS(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	cfg := d.cfg()
	cfg.TunnelID = "tid"
	d.Store.SaveConfig(cfg)
	b, _ := json.Marshal(tunnel.OriginCert{ZoneID: "z1", APIToken: "tok"})
	os.MkdirAll(d.zoneCertDir(), 0o700)
	os.WriteFile(filepath.Join(d.zoneCertDir(), "example.com.pem"), []byte("-----BEGIN ARGO TUNNEL TOKEN-----\n"+base64.StdEncoding.EncodeToString(b)+"\n-----END ARGO TUNNEL TOKEN-----\n"), 0o600)
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recs := []map[string]string{}
		if r.URL.Query().Get("name") == "auth.example.com" {
			recs = append(recs, map[string]string{"type": "A", "name": "auth.example.com", "content": "66.228.47.77"})
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": recs})
	}))
	defer cf.Close()
	d.CloudflareAPI = func(tok string) *tunnel.API { a := tunnel.NewAPI(tok); a.BaseURL = cf.URL; return a }
	ctx := context.Background()
	if err := d.checkSignInHost(ctx, d.cfg(), "auth.example.com"); err == nil || !strings.Contains(err.Error(), "A 66.228.47.77") {
		t.Fatalf("auth host with its own A record: %v", err)
	}
	if err := d.checkSignInHost(ctx, d.cfg(), "signin.example.com"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a.b.example.com", "signin.other.com", "hooks.example.com"} {
		if err := d.checkSignInHost(ctx, d.cfg(), bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestSetRepoKeepsTheAppAndChangesTheSource(t *testing.T) {
	d, _, repo := newTestDaemon(t)
	ctx := context.Background()
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo, NoDeploy: true})
	if _, err := d.hCreate(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, _ := d.Store.App("web")
	other := t.TempDir()
	for _, bad := range []SetRepoParams{{App: "web", Repo: "--upload-pack=x"}, {App: "web", Repo: other, Branch: "-x"}, {App: "web"}} {
		b, _ := json.Marshal(bad)
		if _, err := d.hSetRepo(ctx, b, io.Discard); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	b, _ = json.Marshal(SetRepoParams{App: "web", Repo: other, Branch: "trunk"})
	if _, err := d.hSetRepo(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	a, _ := d.Store.App("web")
	if a.Repo != other || a.Branch != "trunk" || a.Webhook.Secret == before.Webhook.Secret || a.CreatedAt != before.CreatedAt {
		t.Fatalf("app after set-repo: %+v", a)
	}
	out, _ := exec.Command("git", "--git-dir", d.Store.Path("apps", "web", "repo.git"), "remote", "get-url", "origin").Output()
	if strings.TrimSpace(string(out)) != other {
		t.Fatalf("mirror origin %q", out)
	}
}
