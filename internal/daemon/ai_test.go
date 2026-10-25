package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dps/berth/internal/ai"
)

func newAIDaemon(t *testing.T) (*Daemon, string) {
	d, _, repo := newTestDaemon(t)
	ln, _ := net.Listen("tcp", ":0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg, _ := d.Store.Config()
	cfg.Gateway, cfg.AIPort = "192.168.65.1", port
	d.Store.SaveConfig(cfg)
	d.ai = ai.New(ai.OpenUsage(t.TempDir()))
	return d, repo
}

func (d *Daemon) aiGet(token string) int {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	d.ai.ServeHTTP(w, req)
	return w.Code
}

func TestAIEnableDisable(t *testing.T) {
	d, repo := newAIDaemon(t)
	ctx := context.Background()
	call := func(h func(context.Context, json.RawMessage, io.Writer) (any, error), p any) error {
		b, _ := json.Marshal(p)
		_, err := h(ctx, b, io.Discard)
		return err
	}
	if err := call(d.hCreate, CreateParams{Name: "web", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	if err := call(d.hAIEnable, AIEnableParams{App: "web"}); err == nil || !strings.Contains(err.Error(), "ai:setup") {
		t.Fatalf("enable before setup: %v", err)
	}
	if err := call(d.hAISetup, AISetupParams{Model: "qwen3:8b"}); err != nil {
		t.Fatal(err)
	}
	ac, _ := d.Store.AIConfig()
	if ac.Aliases["default"] != "local/qwen3:8b" {
		t.Fatalf("aliases %v", ac.Aliases)
	}
	if err := call(d.hAIEnable, AIEnableParams{App: "web", Models: []string{"default", "anthropic/*"}, RPM: 30}); err != nil {
		t.Fatal(err)
	}
	cfg := d.cfg()
	env, _ := d.Store.Env("web")
	base := fmt.Sprintf("http://192.168.65.1:%d", cfg.AIPort)
	token := env["OPENAI_API_KEY"]
	if env["OPENAI_BASE_URL"] != base+"/v1" || env["ANTHROPIC_BASE_URL"] != base+"/anthropic" || env["ANTHROPIC_API_KEY"] != token || !strings.HasPrefix(token, ai.TokenPrefix) {
		t.Fatalf("env %v", env)
	}
	a, _ := d.Store.App("web")
	if a.AI == nil || a.AI.TokenHash != ai.HashToken(token) || a.AI.RPM != 30 || len(a.AI.Models) != 2 {
		t.Fatalf("app ai %+v", a.AI)
	}
	if code := d.aiGet(token); code != 200 {
		t.Fatalf("gateway refused the app's token: %d", code)
	}
	// The release the app runs has the variables (config release).
	if rel := d.Store.CurrentRelease("web"); rel == nil || rel.Env["OPENAI_API_KEY"] != token {
		t.Fatal("not redeployed with the gateway variables")
	}
	// Caddy listens for apps on the gateway port, for the container network.
	b, _ := os.ReadFile(d.Store.Path("caddy.json"))
	if !strings.Contains(string(b), fmt.Sprintf(`":%d"`, cfg.AIPort)) || !strings.Contains(string(b), "192.168.65.0/24") || !strings.Contains(string(b), cfg.AIAddr) {
		t.Fatalf("caddy config has no AI server:\n%s", b)
	}
	// Enabling again keeps the token and updates the limits.
	call(d.hAIEnable, AIEnableParams{App: "web"})
	env, _ = d.Store.Env("web")
	if a, _ = d.Store.App("web"); env["OPENAI_API_KEY"] != token || a.AI.RPM != 0 || len(a.AI.Models) != 0 {
		t.Fatalf("re-enable rotated the token or kept old limits: %+v", a.AI)
	}

	// A dev version gets its own token, for local models only.
	sha := snapshotInto(t, d, repo, "web")
	b, _ = json.Marshal(DevDeployParams{App: "web", Ref: sha})
	if _, err := d.hDevDeploy(ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	devEnv, _ := d.Store.Env("web-dev")
	dev, _ := d.Store.App("web-dev")
	devToken := devEnv["OPENAI_API_KEY"]
	if devToken == token || dev.AI == nil || dev.AI.TokenHash != ai.HashToken(devToken) || strings.Join(dev.AI.Models, ",") != "local/*" {
		t.Fatalf("dev ai %+v token same as prod: %v", dev.AI, devToken == token)
	}
	if code := d.aiGet(devToken); code != 200 {
		t.Fatalf("dev token refused: %d", code)
	}

	// An app with its own key isn't overwritten without --replace.
	if err := call(d.hCreate, CreateParams{Name: "other", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	oenv, _ := d.Store.Env("other")
	oenv["ANTHROPIC_API_KEY"] = "sk-ant-own"
	d.Store.SaveEnv("other", oenv)
	if err := call(d.hAIEnable, AIEnableParams{App: "other"}); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("overwrote the app's own key: %v", err)
	}

	// Disabling revokes the token and removes the variables.
	if err := call(d.hAIDisable, AppParams{App: "web"}); err != nil {
		t.Fatal(err)
	}
	env, _ = d.Store.Env("web")
	a, _ = d.Store.App("web")
	for _, k := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"} {
		if _, ok := env[k]; ok {
			t.Fatalf("%s left behind", k)
		}
	}
	if a.AI != nil || d.aiGet(token) != http.StatusUnauthorized {
		t.Fatal("token still works after disable")
	}
}

func TestAIKeysAndAliases(t *testing.T) {
	d, _ := newAIDaemon(t)
	ctx := context.Background()
	call := func(h func(context.Context, json.RawMessage, io.Writer) (any, error), p any) error {
		b, _ := json.Marshal(p)
		_, err := h(ctx, b, io.Discard)
		return err
	}
	if err := call(d.hAIKey, AIKeyParams{Provider: "anthropic", Key: "sk-ant-x"}); err != nil {
		t.Fatal(err)
	}
	if err := call(d.hAIKey, AIKeyParams{Provider: "nosuch", Key: "k"}); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("unknown provider: %v", err)
	}
	if err := call(d.hAIKey, AIKeyParams{Provider: "lmstudio", URL: "http://studio.local:1234/v1"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(d.aiKeysPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("keys file: %v %v", fi, err)
	}
	if err := call(d.hAIAlias, AIAliasParams{Name: "smart", Model: "anthropic/claude-sonnet-5-5"}); err != nil {
		t.Fatal(err)
	}
	if err := call(d.hAIAlias, AIAliasParams{Name: "studio", Model: "lmstudio/qwen"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []AIAliasParams{{Name: "x", Model: "smart"}, {Name: "x", Model: "nope/m"}, {Name: "Bad Name", Model: "local/a"}} {
		if err := call(d.hAIAlias, bad); err == nil {
			t.Fatalf("accepted alias %+v", bad)
		}
	}
	st, err := d.aiStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range st.Providers {
		names = append(names, fmt.Sprintf("%s:%v", p.Name, p.HasKey))
	}
	if strings.Join(names, " ") != "anthropic:true lmstudio:false" || st.Aliases["smart"] == "" {
		t.Fatalf("status providers %v aliases %v", names, st.Aliases)
	}
}
