package ai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/state"
)

// TokenPrefix marks the per-app tokens apps send in place of a provider key.
const TokenPrefix = "berth_ai_"

const maxBody = 32 << 20

// NewToken returns a fresh app token and the hash berthd stores.
func NewToken() (token, hash string) {
	b := make([]byte, 24)
	rand.Read(b)
	token = TokenPrefix + hex.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// App is an app allowed to use the gateway.
type App struct {
	Name   string
	Policy state.AppAI
}

// Gateway serves the OpenAI-compatible API (/v1/...) and the Anthropic
// Messages API (/anthropic/v1/...) to apps, forwarding to Ollama or a cloud
// provider with berthd's key.
type Gateway struct {
	Usage  *Usage
	Client *http.Client
	// QueueWait bounds how long a request waits for a local model slot.
	QueueWait time.Duration

	mu    sync.RWMutex
	cfg   *state.AIConfig
	keys  map[string]string
	apps  map[string]App // token hash → app
	local chan struct{}
	rate  map[string][]time.Time
}

func New(usage *Usage) *Gateway {
	g := &Gateway{
		Usage:     usage,
		Client:    &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 10 * time.Minute, IdleConnTimeout: 90 * time.Second}},
		QueueWait: 5 * time.Minute,
		rate:      map[string][]time.Time{},
	}
	g.Configure(&state.AIConfig{}, nil, nil)
	return g
}

// Configure swaps in the current setup; requests in flight keep theirs.
func (g *Gateway) Configure(cfg *state.AIConfig, keys map[string]string, apps []App) {
	byHash := map[string]App{}
	for _, a := range apps {
		if a.Policy.TokenHash != "" {
			byHash[a.Policy.TokenHash] = a
		}
	}
	n := cfg.LocalParallel
	if n <= 0 {
		n = 2
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.local == nil || cap(g.local) != n {
		g.local = make(chan struct{}, n)
	}
	g.cfg, g.keys, g.apps = cfg, keys, byHash
}

func (g *Gateway) snapshot() (*state.AIConfig, map[string]string, chan struct{}) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cfg, g.keys, g.local
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		io.WriteString(w, "ok\n")
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		g.models(w, r)
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions" || r.URL.Path == "/v1/embeddings"):
		g.proxy(w, r, false)
	case r.Method == http.MethodPost && (r.URL.Path == "/anthropic/v1/messages" || r.URL.Path == "/anthropic/v1/messages/count_tokens"):
		g.proxy(w, r, true)
	default:
		apiError(w, strings.HasPrefix(r.URL.Path, "/anthropic/"), http.StatusNotFound, "not_found_error",
			"the Berth AI gateway serves /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models and /anthropic/v1/messages")
	}
}

// apiError answers in the error shape the client's SDK expects.
func apiError(w http.ResponseWriter, anthropic bool, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if anthropic {
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": msg}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "message": msg}})
}

func (g *Gateway) authenticate(r *http.Request) (App, bool) {
	tok := r.Header.Get("X-Api-Key")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	}
	if !strings.HasPrefix(tok, TokenPrefix) {
		return App{}, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	a, ok := g.apps[HashToken(tok)]
	return a, ok
}

// admit resolves the model and applies the app's allow list, budget and
// rate limit.
func (g *Gateway) admit(app App, cfg *state.AIConfig, asked string) (Target, int, string, error) {
	t, err := Resolve(cfg, asked)
	if err != nil {
		return t, http.StatusBadRequest, "invalid_request_error", err
	}
	if !Allowed(app.Policy.Models, asked, t) {
		return t, http.StatusForbidden, "permission_error", fmt.Errorf("%s may not use %s (allowed: %s; change with berth ai:enable %s --models ...)", app.Name, asked, strings.Join(app.Policy.Models, ", "), app.Name)
	}
	if limit := app.Policy.MonthlyTokens; limit > 0 && g.Usage != nil {
		if used := g.Usage.MonthTokens(app.Name); used >= limit {
			return t, http.StatusTooManyRequests, "rate_limit_error", fmt.Errorf("%s has used its %d tokens for this month", app.Name, limit)
		}
	}
	if rpm := app.Policy.RPM; rpm > 0 {
		g.mu.Lock()
		now := time.Now()
		recent := g.rate[app.Name][:0]
		for _, at := range g.rate[app.Name] {
			if now.Sub(at) < time.Minute {
				recent = append(recent, at)
			}
		}
		over := len(recent) >= rpm
		if !over {
			recent = append(recent, now)
		}
		g.rate[app.Name] = recent
		g.mu.Unlock()
		if over {
			return t, http.StatusTooManyRequests, "rate_limit_error", fmt.Errorf("%s is limited to %d requests a minute", app.Name, rpm)
		}
	}
	return t, 0, "", nil
}

// proxy forwards one request. anthropic selects the native Messages API
// (Anthropic models only); otherwise the OpenAI-compatible API, for every
// provider.
func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, anthropic bool) {
	app, ok := g.authenticate(r)
	if !ok {
		apiError(w, anthropic, http.StatusUnauthorized, "authentication_error", "send this app's Berth AI token (OPENAI_API_KEY / ANTHROPIC_API_KEY, set by berth ai:enable)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		apiError(w, anthropic, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body over 32 MB")
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, anthropic, http.StatusBadRequest, "invalid_request_error", "request body is not a JSON object")
		return
	}
	var asked string
	json.Unmarshal(req["model"], &asked)
	if asked == "" {
		apiError(w, anthropic, http.StatusBadRequest, "invalid_request_error", "model is required (an alias such as \"default\", or provider/model)")
		return
	}
	cfg, keys, local := g.snapshot()
	t, status, kind, err := g.admit(app, cfg, asked)
	if err != nil {
		g.record(app, asked, t, 0, 0, time.Now(), status)
		apiError(w, anthropic, status, kind, err.Error())
		return
	}
	if anthropic && t.Provider != "anthropic" {
		apiError(w, true, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("%s is served by %s; the Anthropic API here is for anthropic/ models (use /v1/chat/completions for the rest)", asked, t.Provider))
		return
	}
	key := keys[t.Provider]
	if t.Provider != Local && key == "" {
		apiError(w, anthropic, http.StatusServiceUnavailable, "api_error", fmt.Sprintf("no API key for %s on this Berth (berth ai:key %s)", t.Provider, t.Provider))
		return
	}
	req["model"], _ = json.Marshal(t.Model)
	var stream bool
	json.Unmarshal(req["stream"], &stream)
	// Ask OpenAI-style streams for a final usage chunk so budgets count
	// streamed tokens; drop that chunk again if the app didn't ask for it.
	stripUsage := false
	if stream && !anthropic && r.URL.Path != "/v1/embeddings" {
		opts := map[string]any{}
		json.Unmarshal(req["stream_options"], &opts)
		if v, _ := opts["include_usage"].(bool); !v {
			opts["include_usage"] = true
			req["stream_options"], _ = json.Marshal(opts)
			stripUsage = true
		}
	}
	out, _ := json.Marshal(req)

	if t.Provider == Local {
		select {
		case local <- struct{}{}:
			defer func() { <-local }()
		case <-r.Context().Done():
			return
		case <-time.After(g.QueueWait):
			apiError(w, anthropic, http.StatusServiceUnavailable, "overloaded_error", "local models are busy; try again")
			return
		}
	}
	var url string
	if anthropic {
		url = AnthropicURL + strings.TrimPrefix(r.URL.Path, "/anthropic")
	} else {
		url = BaseURL(cfg, t.Provider) + strings.TrimPrefix(r.URL.Path, "/v1")
	}
	ureq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(out))
	if err != nil {
		apiError(w, anthropic, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	if anthropic {
		ureq.Header.Set("X-Api-Key", key)
		v := r.Header.Get("Anthropic-Version")
		if v == "" {
			v = "2023-06-01"
		}
		ureq.Header.Set("Anthropic-Version", v)
		if b := r.Header.Get("Anthropic-Beta"); b != "" {
			ureq.Header.Set("Anthropic-Beta", b)
		}
	} else if key != "" {
		ureq.Header.Set("Authorization", "Bearer "+key)
	}
	if stream {
		ureq.Header.Set("Accept", "text/event-stream")
	}
	start := time.Now()
	resp, err := g.Client.Do(ureq)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		msg := fmt.Sprintf("%s didn't answer: %v", t.Provider, err)
		if t.Provider == Local {
			msg = fmt.Sprintf("Ollama isn't answering at %s (berth ai:setup starts it): %v", BaseURL(cfg, Local), err)
		}
		g.record(app, asked, t, 0, 0, start, http.StatusBadGateway)
		apiError(w, anthropic, http.StatusBadGateway, "api_error", msg)
		return
	}
	defer resp.Body.Close()
	in, outTok := relay(w, resp, stripUsage)
	g.record(app, asked, t, in, outTok, start, resp.StatusCode)
}

func (g *Gateway) record(app App, asked string, t Target, in, out int64, start time.Time, status int) {
	if g.Usage == nil {
		return
	}
	model := ""
	if t.Provider != "" {
		model = t.String()
	}
	g.Usage.Add(Record{Time: time.Now(), App: app.Name, Asked: asked, Model: model, In: in, Out: out, Ms: time.Since(start).Milliseconds(), Status: status})
}

var relayHeaders = []string{"Content-Type", "Retry-After", "Request-Id", "X-Request-Id"}

// relay copies the provider's answer to the app, streaming server-sent
// events as they arrive, and returns the token counts it reported.
func relay(w http.ResponseWriter, resp *http.Response, stripUsage bool) (in, out int64) {
	for _, h := range relayHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
		w.Write(b)
		in, out, _ = usageOf(b)
		return
	}
	flusher, _ := w.(http.Flusher)
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	var event []string
	emit := func() {
		drop := false
		for _, line := range event {
			data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data:")
			if !ok || !strings.Contains(data, `"usage"`) {
				continue
			}
			i, o, usageOnly := usageOf([]byte(strings.TrimSpace(data)))
			in, out = max(in, i), max(out, o)
			drop = drop || (stripUsage && usageOnly)
		}
		if !drop && len(event) > 0 {
			for _, line := range event {
				io.WriteString(w, line)
			}
			io.WriteString(w, "\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		event = event[:0]
	}
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			if strings.TrimRight(line, "\r\n") == "" {
				emit()
			} else {
				if !strings.HasSuffix(line, "\n") {
					line += "\n"
				}
				event = append(event, line)
			}
		}
		if err != nil {
			emit()
			return
		}
	}
}

type tokenCounts struct {
	PromptTokens             int64 `json:"prompt_tokens"`
	CompletionTokens         int64 `json:"completion_tokens"`
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// usageOf reads token counts from an OpenAI or Anthropic response, stream
// chunk or event. usageOnly is an OpenAI stream's final usage chunk (no
// choices).
func usageOf(b []byte) (in, out int64, usageOnly bool) {
	var doc struct {
		Usage   *tokenCounts `json:"usage"`
		Message *struct {
			Usage *tokenCounts `json:"usage"`
		} `json:"message"` // Anthropic message_start
		Choices *[]json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return 0, 0, false
	}
	u := doc.Usage
	if u == nil && doc.Message != nil {
		u = doc.Message.Usage
	}
	if u == nil {
		return 0, 0, false
	}
	in = u.PromptTokens + u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	out = u.CompletionTokens + u.OutputTokens
	return in, out, doc.Choices != nil && len(*doc.Choices) == 0
}

// models lists what the app may ask for: aliases, then local models.
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	app, ok := g.authenticate(r)
	if !ok {
		apiError(w, false, http.StatusUnauthorized, "authentication_error", "send this app's Berth AI token")
		return
	}
	cfg, _, _ := g.snapshot()
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{}
	seen := map[string]bool{}
	add := func(id, owner string) {
		t, err := Resolve(cfg, id)
		if err != nil || seen[id] || !Allowed(app.Policy.Models, id, t) {
			return
		}
		seen[id] = true
		data = append(data, model{id, "model", owner})
	}
	for _, alias := range sortedKeys(cfg.Aliases) {
		add(alias, "berth alias for "+cfg.Aliases[alias])
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if local, err := OllamaModels(ctx, ollamaURL(cfg)); err == nil {
		for _, m := range local {
			add(Local+"/"+m.Name, "ollama")
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func ollamaURL(cfg *state.AIConfig) string {
	if cfg.OllamaURL != "" {
		return strings.TrimSuffix(cfg.OllamaURL, "/")
	}
	return DefaultOllamaURL
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
