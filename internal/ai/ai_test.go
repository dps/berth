package ai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dps/berth/internal/state"
)

// upstream is a fake provider: it records requests and answers like
// OpenAI (or Ollama's /v1), streaming when asked, or like Anthropic.
type upstream struct {
	mu   sync.Mutex
	reqs []seen
}

type seen struct {
	Path, Auth, APIKey, Version string
	Body                        map[string]any
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	u.mu.Lock()
	u.reqs = append(u.reqs, seen{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version"), body})
	u.mu.Unlock()
	switch {
	case r.URL.Path == "/api/tags":
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "qwen3:8b"}, {"name": "llama3.2:3b"}}})
	case strings.HasSuffix(r.URL.Path, "/v1/messages"):
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"cache_read_input_tokens":100,"output_tokens":3}}`)
	case body["stream"] == true:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] == true {
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":4}}`)
	}
}

func (u *upstream) last() seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[len(u.reqs)-1]
}

type testGateway struct {
	*Gateway
	up     *upstream
	tokens map[string]string // app → token
}

func newTestGateway(t *testing.T) *testGateway {
	up := &upstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	old := AnthropicURL
	AnthropicURL = srv.URL
	t.Cleanup(func() { AnthropicURL = old })
	cfg := &state.AIConfig{
		Aliases:   map[string]string{"default": "local/qwen3:8b", "smart": "anthropic/claude-sonnet-5-5", "cheap": "acme/tiny"},
		Providers: map[string]state.AIProvider{"acme": {URL: srv.URL + "/acme/v1"}},
		OllamaURL: srv.URL,
	}
	g := New(OpenUsage(t.TempDir()))
	tokens := map[string]string{}
	var apps []App
	for name, policy := range map[string]state.AppAI{
		"web":    {},
		"dev":    {Models: []string{"local/*"}},
		"capped": {MonthlyTokens: 10},
		"slow":   {RPM: 1},
	} {
		tok, hash := NewToken()
		policy.TokenHash = hash
		tokens[name] = tok
		apps = append(apps, App{Name: name, Policy: policy})
	}
	g.Configure(cfg, map[string]string{"anthropic": "sk-ant-real", "acme": "acme-key"}, apps)
	return &testGateway{g, up, tokens}
}

func (g *testGateway) do(t *testing.T, app, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if app != "" {
		req.Header.Set("Authorization", "Bearer "+g.tokens[app])
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	return w.Result(), w.Body.String()
}

func TestGatewayRoutesAndRewrites(t *testing.T) {
	g := newTestGateway(t)
	// No token, a wrong token, or a provider key: refused.
	for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer berth_ai_nope"}, {"Authorization": "Bearer sk-ant-real"}} {
		if resp, _ := g.do(t, "", "/v1/chat/completions", `{"model":"default"}`, hdr); resp.StatusCode != 401 {
			t.Fatalf("unauthenticated: %d", resp.StatusCode)
		}
	}
	// An alias reaches Ollama with the real model name and no key.
	resp, body := g.do(t, "web", "/v1/chat/completions", `{"model":"default","messages":[]}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("local: %d %s", resp.StatusCode, body)
	}
	if s := g.up.last(); s.Path != "/v1/chat/completions" || s.Body["model"] != "qwen3:8b" || s.Auth != "" {
		t.Fatalf("local upstream saw %+v", s)
	}
	// A custom provider gets berthd's key, never the app's token.
	g.do(t, "web", "/v1/embeddings", `{"model":"cheap","input":"x"}`, nil)
	if s := g.up.last(); s.Path != "/acme/v1/embeddings" || s.Body["model"] != "tiny" || s.Auth != "Bearer acme-key" {
		t.Fatalf("custom upstream saw %+v", s)
	}
	// Unknown model and provider without a key.
	if resp, body := g.do(t, "web", "/v1/chat/completions", `{"model":"gpt-9"}`, nil); resp.StatusCode != 400 || !strings.Contains(body, "aliases: cheap, default, smart") {
		t.Fatalf("unknown model: %d %s", resp.StatusCode, body)
	}
	if resp, body := g.do(t, "web", "/v1/chat/completions", `{"model":"openai/gpt-x"}`, nil); resp.StatusCode != 503 || !strings.Contains(body, "berth ai:key openai") {
		t.Fatalf("no key: %d %s", resp.StatusCode, body)
	}
	// The allow list: dev may use local models only.
	if resp, body := g.do(t, "dev", "/v1/chat/completions", `{"model":"smart"}`, nil); resp.StatusCode != 403 || !strings.Contains(body, "may not use smart") {
		t.Fatalf("dev using cloud: %d %s", resp.StatusCode, body)
	}
	if resp, _ := g.do(t, "dev", "/v1/chat/completions", `{"model":"default"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("dev using local: %d", resp.StatusCode)
	}
}

func TestGatewayStreamingCountsTokens(t *testing.T) {
	g := newTestGateway(t)
	// The app didn't ask for usage: Berth asks for it, counts it, and keeps
	// the extra chunk from the app.
	resp, body := g.do(t, "web", "/v1/chat/completions", `{"model":"default","stream":true}`, nil)
	if resp.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(body, `"hel"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream: %q", body)
	}
	if strings.Contains(body, "usage") {
		t.Fatalf("usage chunk leaked to an app that didn't ask: %q", body)
	}
	if got := g.Usage.MonthTokens("web"); got != 9 {
		t.Fatalf("streamed tokens counted %d, want 9", got)
	}
	// Asked for: passed through.
	_, body = g.do(t, "web", "/v1/chat/completions", `{"model":"default","stream":true,"stream_options":{"include_usage":true}}`, nil)
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("requested usage chunk dropped: %q", body)
	}
}

func TestGatewayAnthropicPassthrough(t *testing.T) {
	g := newTestGateway(t)
	hdr := map[string]string{"Anthropic-Beta": "tools-2099"}
	resp, body := g.do(t, "web", "/anthropic/v1/messages", `{"model":"smart","max_tokens":10,"messages":[]}`, hdr)
	if resp.StatusCode != 200 || !strings.Contains(body, `"hi"`) {
		t.Fatalf("messages: %d %s", resp.StatusCode, body)
	}
	if s := g.up.last(); s.Path != "/v1/messages" || s.APIKey != "sk-ant-real" || s.Version != "2023-06-01" || s.Body["model"] != "claude-sonnet-5-5" {
		t.Fatalf("anthropic upstream saw %+v", s)
	}
	// Cache reads count as input.
	if got := g.Usage.MonthTokens("web"); got != 115 {
		t.Fatalf("tokens %d, want 115", got)
	}
	// Streaming: input from message_start, output from message_delta.
	g.do(t, "web", "/anthropic/v1/messages", `{"model":"anthropic/claude-haiku-5-5","stream":true}`, map[string]string{"X-Api-Key": g.tokens["web"]})
	if got := g.Usage.MonthTokens("web"); got != 115+29 {
		t.Fatalf("tokens %d, want %d", got, 115+29)
	}
	// Local models aren't served by the Anthropic API here.
	if resp, body := g.do(t, "web", "/anthropic/v1/messages", `{"model":"default"}`, nil); resp.StatusCode != 400 || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("local over anthropic API: %d %s", resp.StatusCode, body)
	}
}

func TestGatewayLimits(t *testing.T) {
	g := newTestGateway(t)
	if resp, _ := g.do(t, "capped", "/v1/chat/completions", `{"model":"default"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	// 9 tokens used of 10: still allowed; 18 of 10: over.
	g.do(t, "capped", "/v1/chat/completions", `{"model":"default"}`, nil)
	if resp, body := g.do(t, "capped", "/v1/chat/completions", `{"model":"default"}`, nil); resp.StatusCode != 429 || !strings.Contains(body, "10 tokens for this month") {
		t.Fatalf("over budget: %d %s", resp.StatusCode, body)
	}
	g.do(t, "slow", "/v1/chat/completions", `{"model":"default"}`, nil)
	if resp, _ := g.do(t, "slow", "/v1/chat/completions", `{"model":"default"}`, nil); resp.StatusCode != 429 {
		t.Fatalf("rate limit: %d", resp.StatusCode)
	}
	rows := g.Usage.Summary(month(time.Now()), "capped")
	if len(rows) != 1 || rows[0].Requests != 3 || rows[0].Errors != 1 || rows[0].Model != "local/qwen3:8b" {
		t.Fatalf("summary %+v", rows)
	}
	// The budget survives a restart (totals reload from the log).
	if again := OpenUsage(g.Usage.dir); again.MonthTokens("capped") != 18 {
		t.Fatalf("reloaded total %d", again.MonthTokens("capped"))
	}
}

func TestGatewayModelsList(t *testing.T) {
	g := newTestGateway(t)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+g.tokens["dev"])
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	// dev sees only local models: the "default" alias and Ollama's list.
	if got := strings.Join(ids, " "); got != "default local/qwen3:8b local/llama3.2:3b" {
		t.Fatalf("models %q", got)
	}
}

func TestRecommendedModel(t *testing.T) {
	if RecommendedModel(8<<30) != "qwen3:4b" || RecommendedModel(16<<30) != "qwen3:8b" || RecommendedModel(64<<30) != "qwen3:8b" {
		t.Fatal("recommendation")
	}
}
