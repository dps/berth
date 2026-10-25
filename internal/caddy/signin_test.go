package caddy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
)

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// staticUpstreams swaps the SRV lookup for a fixed upstream so the test
// needs no DNS responder.
func staticUpstreams(v any, dial string) {
	switch x := v.(type) {
	case obj:
		if _, ok := x["dynamic_upstreams"]; ok {
			delete(x, "dynamic_upstreams")
			x["upstreams"] = []any{obj{"dial": dial}}
		}
		for _, c := range x {
			staticUpstreams(c, dial)
		}
	case []any:
		for _, c := range x {
			staticUpstreams(c, dial)
		}
	}
}

// TestSignInThroughRealCaddy runs the generated config in a real Caddy: a
// forged identity header never reaches the app, protected paths redirect
// to sign in, a signed-in request gets berthd's headers, POST bodies
// survive forward_auth, and /_berth/* reaches berthd.
func TestSignInThroughRealCaddy(t *testing.T) {
	bin, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("caddy not installed")
	}
	// The app: echoes the identity headers and the body it received.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]string{
			"email": r.Header.Get("X-Berth-Email"), "id": r.Header.Get("X-Berth-User-Id"),
			"identity": r.Header.Get("X-Berth-Identity"), "body": string(body), "method": r.Method,
		})
	}))
	defer app.Close()

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	as := auth.NewServer(auth.OpenSessions(filepath.Join(t.TempDir(), "s.json")), key)
	as.Configure(nil, "auth.example.com", nil)
	access := &state.Access{Level: state.LevelPublic, Paths: []state.PathRule{{Path: "/private/*", Level: state.LevelSignedIn}}}
	as.SetSites(map[string]auth.Site{"chirp.example.com": {App: "chirp", BaseURL: "https://chirp.example.com", Access: access}})
	authAddr := freeAddr(t)
	authLn, err := net.Listen("tcp", authAddr)
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(authLn, as)
	defer authLn.Close()
	cookie := as.Sessions.Create("chirp.example.com", auth.Identity{Provider: "google", Subject: "7", Email: "alice@gmail.com"})

	admin, listen := freeAddr(t), freeAddr(t)
	cfg := Build(Options{
		Admin: admin, HTTPListen: listen, DNSAddr: "127.0.0.1:1", SRVDomain: "berth.internal",
		AuthAddr: authAddr, AuthHost: "auth.example.com", IDHeaders: auth.Headers,
		Routes: []Route{
			{App: "chirp", PublicHosts: []string{"chirp.example.com"}, Service: "chirp", SignIn: true, Private: true},
			{App: "plain", PublicHosts: []string{"plain.example.com"}, Service: "plain"},
		},
	})
	staticUpstreams(cfg, strings.TrimPrefix(app.URL, "http://"))
	b, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "caddy.json")
	os.WriteFile(path, b, 0o600)
	cmd := exec.Command(bin, "run", "--config", path)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_DATA_HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for NewClient(admin).Ping(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatalf("caddy didn't start: %s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, host, path, body string, hdr map[string]string) (*http.Response, map[string]string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+listen+path, strings.NewReader(body))
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]string{}
		raw, _ := io.ReadAll(resp.Body)
		json.Unmarshal(raw, &out)
		return resp, out
	}
	forged := map[string]string{"X-Berth-Email": "ceo@example.com", "X-Berth-User-Id": "google:1", "Accept": "text/html"}

	// Public path, anonymous, forged headers: they are stripped.
	resp, got := do("GET", "chirp.example.com", "/", "", forged)
	if resp.StatusCode != 200 || got["email"] != "" || got["id"] != "" {
		t.Fatalf("anonymous public request: %d %v", resp.StatusCode, got)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("Cache-Control %q", cc)
	}
	// An app without a policy strips them too.
	if _, got := do("GET", "plain.example.com", "/", "", forged); got["email"] != "" {
		t.Fatalf("plain app got a forged header: %v", got)
	}
	// Protected path, anonymous: redirect to sign in, app never sees it.
	resp, _ = do("GET", "chirp.example.com", "/private/x", "", forged)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/_berth/login?return=") {
		t.Fatalf("protected anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Signed in: berthd's headers replace forged ones, and a POST body
	// reaches the app intact after forward_auth.
	hdr := map[string]string{"Cookie": "__Host-berth_session=" + cookie, "X-Berth-Email": "ceo@example.com", "Content-Type": "application/json"}
	resp, got = do("POST", "chirp.example.com", "/private/post", `{"body":"hello"}`, hdr)
	if resp.StatusCode != 200 || got["email"] != "alice@gmail.com" || got["id"] != "google:7" || got["body"] != `{"body":"hello"}` || got["method"] != "POST" || got["identity"] == "" {
		t.Fatalf("signed-in POST: %d %v", resp.StatusCode, got)
	}
	// /_berth/* is answered by berthd.
	resp, _ = do("GET", "chirp.example.com", "/_berth/me", "", hdr)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("/_berth/me: %d", resp.StatusCode)
	}
	// The auth host is routed to berthd.
	resp, _ = do("GET", "auth.example.com", "/", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("auth host: %d", resp.StatusCode)
	}
}
