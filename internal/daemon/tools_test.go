package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/caddy"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tools"
)

func TestToolsEnableDisable(t *testing.T) {
	d, repo := newAIDaemon(t)
	ctx := context.Background()
	call := func(h handler, p any) error {
		b, _ := json.Marshal(p)
		_, err := h(ctx, b, io.Discard)
		return err
	}
	if err := call(d.hCreate, CreateParams{Name: "web", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	if err := call(d.hToolsEnable, ToolsEnableParams{App: "web", Tools: []string{"wbe.*"}, NoRestart: true}); err == nil || !strings.Contains(err.Error(), "no tool matches") {
		t.Fatalf("a typo in --tools: %v", err)
	}
	if err := call(d.hToolsEnable, ToolsEnableParams{App: "web", Tools: []string{"web.*"}, RPM: 5, MonthlyCalls: 100, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := d.Store.App("web")
	env, _ := d.Store.Env("web")
	token := env[envToolsKey]
	if !strings.HasPrefix(token, tools.TokenPrefix) || a.Tools.TokenHash != tools.HashToken(token) || a.Tools.RPM != 5 || a.Tools.KeylessOnly {
		t.Fatalf("token %q policy %+v", token, a.Tools)
	}
	if !strings.HasSuffix(env[envToolsURL], "/tools") || !strings.Contains(env[envToolsURL], "192.168.65.1:") {
		t.Fatalf("url %q", env[envToolsURL])
	}
	if !strings.Contains(mustRead(t, d.Store.Path("caddy.json")), `"strip_path_prefix": "/tools"`) {
		t.Fatal("Caddy doesn't route /tools")
	}

	// Re-enabling keeps the token and replaces the limits.
	if err := call(d.hToolsEnable, ToolsEnableParams{App: "web", NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	env, _ = d.Store.Env("web")
	if a, _ = d.Store.App("web"); env[envToolsKey] != token || a.Tools.RPM != 0 || len(a.Tools.Tools) != 0 {
		t.Fatalf("re-enable rotated the token or kept old limits: %+v", a.Tools)
	}

	// A dev version gets its own token, limited to keyless providers.
	sha := snapshotInto(t, d, repo, "web")
	if err := call(d.hDevDeploy, DevDeployParams{App: "web", Ref: sha}); err != nil {
		t.Fatal(err)
	}
	devEnv, _ := d.Store.Env("web-dev")
	dev, _ := d.Store.App("web-dev")
	if devEnv[envToolsKey] == token || dev.Tools == nil || !dev.Tools.KeylessOnly || dev.Tools.TokenHash != tools.HashToken(devEnv[envToolsKey]) {
		t.Fatalf("dev tools %+v", dev.Tools)
	}
	if err := call(d.hToolsEnable, ToolsEnableParams{App: "web-dev", Full: true, NoRestart: true}); err != nil {
		t.Fatal(err)
	}
	if dev, _ = d.Store.App("web-dev"); dev.Tools.KeylessOnly {
		t.Fatal("--full didn't lift the limit")
	}

	if err := call(d.hToolsDisable, AppParams{App: "web"}); err != nil {
		t.Fatal(err)
	}
	env, _ = d.Store.Env("web")
	a, _ = d.Store.App("web")
	if a.Tools != nil || env[envToolsKey] != "" || env[envToolsURL] != "" {
		t.Fatalf("not disabled: %+v %v", a.Tools, env)
	}
}

func TestToolsKeysAndRoutes(t *testing.T) {
	d, _ := newAIDaemon(t)
	ctx := context.Background()
	call := func(h handler, p any) error {
		b, _ := json.Marshal(p)
		_, err := h(ctx, b, io.Discard)
		return err
	}
	if err := call(d.hToolsKey, ToolsKeyParams{Provider: "nope", Key: "k"}); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider: %v", err)
	}
	if err := call(d.hToolsKey, ToolsKeyParams{Provider: "openmeteo", Key: "k"}); err == nil {
		t.Fatal("a keyless provider took a key")
	}
	if err := call(d.hToolsKey, ToolsKeyParams{Provider: "ebay", Key: "justone"}); err == nil || !strings.Contains(err.Error(), "AppID:CertID") {
		t.Fatalf("bad eBay key: %v", err)
	}
	daily := 5000
	if err := call(d.hToolsKey, ToolsKeyParams{Provider: "ebay", Key: "id:secret", Daily: &daily}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(d.Store.Path("secrets", "tools.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v %v", fi, err)
	}
	cfg, _ := d.Store.ToolsConfig()
	if d.Store.ToolKeys()["ebay"] != "id:secret" || cfg.Budgets["ebay"] != 5000 {
		t.Fatalf("%+v", cfg)
	}

	if err := call(d.hToolsUse, ToolsUseParams{Tool: "web.search", Providers: []string{"ebay"}}); err == nil {
		t.Fatal("a provider that can't serve the tool was accepted")
	}
	if err := call(d.hToolsUse, ToolsUseParams{Tool: "web.search", Providers: []string{"serpapi", "exa"}}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = d.Store.ToolsConfig()
	if strings.Join(cfg.Routes["web.search"], ",") != "serpapi,exa" {
		t.Fatalf("%+v", cfg)
	}

	st, err := d.hToolsStatus(ctx, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "id:secret") {
		t.Fatal("status holds a key")
	}
	if err := call(d.hToolsKey, ToolsKeyParams{Provider: "ebay", Remove: true}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = d.Store.ToolsConfig(); d.Store.ToolKeys()["ebay"] != "" || cfg.Budgets["ebay"] != 0 {
		t.Fatal("not removed")
	}
}

func TestToolsShareTheAIGatewaysKeys(t *testing.T) {
	d, _ := newAIDaemon(t)
	ctx := context.Background()
	if err := d.saveAIKeys(map[string]string{"openai": "sk-from-ai"}); err != nil {
		t.Fatal(err)
	}
	res, err := d.hToolsStatus(ctx, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	st := res.(*ToolsStatus)
	speak := false
	for _, ts := range st.Tools {
		speak = speak || ts.Name == "audio.speak" && len(ts.Providers) == 1 && ts.Providers[0] == "openai"
	}
	shared := false
	for _, p := range st.Providers {
		shared = shared || p.Name == "openai" && p.HasKey && p.Shared
	}
	if !speak || !shared {
		t.Fatalf("%+v", st)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "sk-from-ai") {
		t.Fatal("status holds a key")
	}
	// Its own key wins, and removing it leaves the AI gateway's alone.
	b, _ = json.Marshal(ToolsKeyParams{Provider: "openai", Key: "sk-own"})
	if _, err := d.hToolsKey(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := tools.Keys(d.Store, "")["openai"]; got != "sk-own" {
		t.Fatal(got)
	}
	b, _ = json.Marshal(ToolsKeyParams{Provider: "openai", Remove: true})
	if _, err := d.hToolsKey(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := tools.Keys(d.Store, "")["openai"]; got != "sk-from-ai" {
		t.Fatal(got)
	}
	if _, ok := tools.Keys(d.Store, "tok")["ai"]; ok {
		t.Fatal("no model is set up, so the AI gateway isn't ready")
	}
	d.Store.SaveAIConfig(&state.AIConfig{Aliases: map[string]string{"default": "local/x"}})
	if got := tools.Keys(d.Store, "tok")["ai"]; got != "tok" {
		t.Fatal(got)
	}
}

func TestToolsDocsFollowTheApp(t *testing.T) {
	d, repo := newAIDaemon(t)
	ctx := context.Background()
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(ToolsDocsParams{App: "web"})
	if _, err := d.hToolsDocs(ctx, b, io.Discard); err == nil || !strings.Contains(err.Error(), "tools:enable web") {
		t.Fatalf("docs before enable: %v", err)
	}
	a, _ := d.Store.App("web")
	a.Tools = &state.AppTools{Tools: []string{"weather.*"}}
	d.Store.SaveApp(a)
	res, err := d.hToolsDocs(ctx, b, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	md := res.(map[string]string)["markdown"]
	if !strings.Contains(md, "## weather.current") || strings.Contains(md, "## web.search") {
		t.Fatal(md)
	}
}

func TestCaddyServesToolsOnlyToContainers(t *testing.T) {
	conf := caddy.Build(caddy.Options{AIListen: ":7790", AIAddr: "127.0.0.1:7782", AISources: []string{"192.168.65.0/24"}, ToolsAddr: "127.0.0.1:7784"})
	b, _ := json.Marshal(conf)
	s := string(b)
	if !strings.Contains(s, `"path":["/tools/*"]`) || !strings.Contains(s, `"remote_ip":{"ranges":["192.168.65.0/24"]}`) || !strings.Contains(s, `"dial":"127.0.0.1:7784"`) {
		t.Fatal(s)
	}
	conf = caddy.Build(caddy.Options{AIListen: ":7790", AIAddr: "127.0.0.1:7782", AISources: []string{"192.168.65.0/24"}})
	b, _ = json.Marshal(conf)
	if strings.Contains(string(b), "/tools") {
		t.Fatal("tools routed while off")
	}
}

func TestToolsChildIsSupervised(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	exe := filepath.Join(dir, "child")
	// Like berthd tools: logs starts and SIGHUPs, and exits when stdin closes.
	script := "#!/bin/sh\necho \"start $1\" >> " + log + "\ntrap 'echo hup >> " + log + "' HUP\ncat <&0 >/dev/null &\nwhile kill -0 $! 2>/dev/null; do wait $!; done\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &toolsChild{exe: exe, want: make(chan bool, 1)}
	go c.supervise(ctx)
	waitFor := func(what string, ok func(lines []string) bool) {
		t.Helper()
		for range 200 {
			b, _ := os.ReadFile(log)
			if ok(strings.Fields(strings.ReplaceAll(string(b), "start tools", "start"))) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		b, _ := os.ReadFile(log)
		t.Fatalf("%s: %q", what, b)
	}
	countOf := func(word string, n int) func([]string) bool {
		return func(lines []string) bool { return countWord(lines, word) >= n }
	}
	running := func() *os.Process {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.proc
	}

	c.set(true)
	waitFor("first start", countOf("start", 1))
	time.Sleep(100 * time.Millisecond) // the trap is set
	c.set(true)
	waitFor("reload", countOf("hup", 1))

	// Killed: it comes back.
	running().Kill()
	waitFor("restart after a crash", countOf("start", 2))

	// Not wanted: it stops, by its stdin closing, and stays stopped.
	for range 100 {
		if running() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.set(false)
	for range 200 {
		if running() == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if running() != nil {
		t.Fatal("still running when not wanted")
	}
	c.set(true)
	waitFor("start when wanted again", countOf("start", 3))
}

func countWord(lines []string, word string) int {
	n := 0
	for _, l := range lines {
		if l == word {
			n++
		}
	}
	return n
}

func mustRead(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
