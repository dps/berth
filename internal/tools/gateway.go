package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/state"
)

// TokenPrefix marks the per-app tokens apps send to the gateway.
const TokenPrefix = "berth_tools_"

// Owner is the app name on calls from `berth tools:call`.
const Owner = "@owner"

// ProviderHeader names the one provider to use on an owner's call, skipping
// the cache, so a test of a key really tests that key.
const ProviderHeader = "X-Berth-Provider"

const (
	maxBody     = 1 << 20
	callTimeout = 30 * time.Second
)

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
	Policy state.AppTools
	all    bool
}

// Error is a failure with an answer for the app. The message names the
// fix when there is one.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func errorf(status int, code, format string, a ...any) *Error {
	return &Error{status, code, fmt.Sprintf(format, a...)}
}

func badInput(format string, a ...any) *Error {
	return errorf(http.StatusBadRequest, "invalid_input", format, a...)
}

func notFound(format string, a ...any) *Error {
	return errorf(http.StatusNotFound, "not_found", format, a...)
}

// Call is what a provider function gets to do its work.
type Call struct {
	Key    string // the provider's key
	HTTP   *http.Client
	Public *http.Client // for URLs that came from an app
	App    string
	Files  *Files
	// AIURL and AIModel reach the AI gateway, with Key as the token.
	AIURL   string
	AIModel string
	hosts   map[string]string
}

// url points a provider URL at a test server when one is set.
func (c *Call) url(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if h, ok := c.hosts[u.Host]; ok {
		t, _ := url.Parse(h)
		u.Scheme, u.Host = t.Scheme, t.Host
	}
	return u.String()
}

// statusError is a provider's non-2xx answer.
type statusError struct {
	Status int
}

func (e *statusError) Error() string { return fmt.Sprintf("answered %d", e.Status) }

// do sends a request to a provider and decodes its JSON answer. Errors
// carry no URL or body, either of which could hold the key.
func (c *Call) do(req *http.Request, into any) error {
	body, _, err := c.fetch(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errors.New("answered with something other than JSON")
	}
	return nil
}

// fetch sends a request to a provider and returns its answer, which can be
// a whole video, and its media type.
func (c *Call) fetch(req *http.Request) ([]byte, string, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// For the owner's log, not the app: say why, without the key.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		slog.Warn("tool provider answered", "status", resp.StatusCode, "host", req.URL.Host, "detail", strings.ReplaceAll(string(detail), c.Key, "<key>"))
		return nil, "", &statusError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFile))
	if err != nil {
		return nil, "", fmt.Errorf("reading the answer: %w", err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func (c *Call) get(ctx context.Context, raw string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(raw), nil)
	if err != nil {
		return errors.New("bad request")
	}
	return c.do(req, into)
}

// Gateway serves the tools API to apps.
type Gateway struct {
	Usage  *Usage
	HTTP   *http.Client
	Public *http.Client
	// Hosts redirects provider hosts, for tests.
	Hosts map[string]string
	// Timeout bounds one provider call, for tools that don't set their own.
	Timeout time.Duration
	Files   *Files
	// BaseURL is the gateway as apps see it, for links to files.
	BaseURL string
	// AIURL reaches the AI gateway; AIModel is the alias tools ask for.
	AIURL   string
	AIModel string

	mu    sync.RWMutex
	cfg   *state.ToolsConfig
	keys  map[string]string
	apps  map[string]App // token hash → app
	admin string         // hash of the token berthd uses for tools:call
	rate  map[string][]time.Time

	cache *cache
	jobs  *jobs
}

func New(usage *Usage) *Gateway {
	g := &Gateway{
		Usage:   usage,
		HTTP:    &http.Client{},
		Public:  publicClient(),
		Timeout: callTimeout,
		rate:    map[string][]time.Time{},
		cache:   newCache(),
		Files:   newFiles(filepath.Join(usage.dir, "files")),
		jobs:    newJobs(filepath.Join(usage.dir, "jobs")),
	}
	g.Files.base = func() string { return g.BaseURL }
	g.Configure(&state.ToolsConfig{}, nil, nil)
	return g
}

// SetAdmin sets the token that calls as the owner.
func (g *Gateway) SetAdmin(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.admin = HashToken(token)
}

// Configure swaps in the current setup; calls in flight keep theirs.
func (g *Gateway) Configure(cfg *state.ToolsConfig, keys map[string]string, apps []App) {
	byHash := map[string]App{}
	for _, a := range apps {
		if a.Policy.TokenHash != "" {
			byHash[a.Policy.TokenHash] = a
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg, g.keys, g.apps = cfg, keys, byHash
}

func (g *Gateway) authenticate(r *http.Request) (App, bool) {
	h, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return App{}, false
	}
	hash := HashToken(h)
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.admin != "" && hash == g.admin {
		return App{Name: Owner}, true
	}
	a, ok := g.apps[hash]
	return a, ok
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		io.WriteString(w, "ok\n")
		return
	}
	app, ok := g.authenticate(r)
	if !ok {
		writeError(w, errorf(http.StatusUnauthorized, "unauthorized", "send the app's BERTH_TOOLS_KEY as a Bearer token (berth tools:enable <app>)"))
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tools":
		writeJSON(w, http.StatusOK, g.listing(app))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/openapi.json":
		writeJSON(w, http.StatusOK, g.openAPI(app))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/docs":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		io.WriteString(w, g.docs(app))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/files":
		g.serveUpload(w, r, app)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/files/"):
		g.serveFile(w, r, app, strings.TrimPrefix(r.URL.Path, "/v1/files/"))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
		g.serveJob(w, r, app, strings.TrimPrefix(r.URL.Path, "/v1/jobs/"))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/"):
		name := strings.Replace(strings.TrimPrefix(r.URL.Path, "/v1/"), "/", ".", 1)
		g.serveCall(w, r, app, name)
	default:
		writeError(w, notFound("POST a tool, like /v1/web/search; GET /v1/tools lists them"))
	}
}

// Meta says how a call was served.
type Meta struct {
	Tool     string `json:"tool"`
	Provider string `json:"provider"`
	Cached   bool   `json:"cached"`
	Ms       int64  `json:"ms"`
	Job      bool   `json:"job,omitempty"` // the answer is a job to check on
}

func (g *Gateway) serveCall(w http.ResponseWriter, r *http.Request, app App, name string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, errorf(http.StatusRequestEntityTooLarge, "too_large", "a call can be at most 1 MB"))
		return
	}
	start := time.Now()
	force := ""
	if app.Name == Owner {
		force = r.Header.Get(ProviderHeader)
	}
	result, meta, cerr := g.call(r.Context(), app, name, body, force)
	meta.Ms = time.Since(start).Milliseconds()
	if cerr != nil {
		writeError(w, cerr)
		return
	}
	if meta.Job {
		writeJSON(w, http.StatusAccepted, map[string]any{"job": result, "meta": meta})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "meta": meta})
}

// call builds what a provider function needs.
func (g *Gateway) newCall(app string, key string) *Call {
	return &Call{Key: key, HTTP: g.HTTP, Public: g.Public, App: app, Files: g.Files, AIURL: g.AIURL, AIModel: g.AIModel, hosts: g.Hosts}
}

// call runs one tool for an app: permission, rate limit, cache, monthly
// limit, then each ready provider in turn.
func (g *Gateway) call(ctx context.Context, app App, name string, body []byte, force string) (json.RawMessage, Meta, *Error) {
	meta := Meta{Tool: name}
	start := time.Now()
	record := func(provider string, cached bool, status int, began time.Time) {
		g.Usage.Add(Record{Time: time.Now(), App: app.Name, Tool: name, Provider: provider, Cached: cached, Ms: time.Since(began).Milliseconds(), Status: status})
	}
	refuse := func(e *Error) (json.RawMessage, Meta, *Error) {
		record("", false, e.Status, start)
		return nil, meta, e
	}

	t := Find(name)
	if t == nil {
		return nil, meta, notFound("no tool %s (GET /v1/tools lists them)", name)
	}
	if !Allowed(app.Policy.Tools, t) && app.Name != Owner {
		allowed := strings.Join(app.Policy.Tools, ", ")
		if allowed == "" {
			allowed = "everything but costly tools"
		}
		return refuse(errorf(http.StatusForbidden, "not_allowed", "%s may not use %s (allowed: %s; change with berth tools:enable %s --tools ...)", app.Name, t.Name, allowed, app.Name))
	}
	if e := g.admitRate(app); e != nil {
		return refuse(e)
	}
	var raw map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return refuse(badInput("the body must be a JSON object"))
		}
	}
	args, err := clean(t.Input, raw)
	if err != nil {
		return refuse(badInput("%s", err))
	}

	key := cacheKey(t.Name, args)
	if t.Cache > 0 && force == "" {
		if hit, ok := g.cache.get(key); ok {
			meta.Provider, meta.Cached = hit.provider, true
			record(hit.provider, true, http.StatusOK, start)
			return hit.body, meta, nil
		}
	}
	if limit := app.Policy.MonthlyCalls; limit > 0 && g.Usage.MonthCalls(app.Name) >= limit {
		return refuse(errorf(http.StatusTooManyRequests, "monthly_limit", "%s has used its %d calls for this month (change with berth tools:enable %s --monthly-calls ...)", app.Name, limit, app.Name))
	}

	g.mu.RLock()
	cfg, keys := g.cfg, g.keys
	g.mu.RUnlock()
	route := Route(t, cfg.Routes, keys)
	if force != "" {
		if !slices.Contains(route, force) {
			return refuse(errorf(http.StatusServiceUnavailable, "no_provider", "%s has no key yet (berth tools:key %s)", force, force))
		}
		route = []string{force}
	}
	if app.Policy.KeylessOnly {
		route = keylessOnly(route)
		if len(route) == 0 {
			return refuse(errorf(http.StatusForbidden, "keyless_only", "dev versions only use keyless tools, and %s needs a key (berth tools:enable %s --full lifts this)", t.Name, app.Name))
		}
	}
	if len(route) == 0 {
		return refuse(errorf(http.StatusServiceUnavailable, "no_provider", "%s has no provider yet (berth tools:key %s)", t.Name, t.Order[0]))
	}

	var last error
	spent := false
	for _, n := range route {
		if b := cfg.Budgets[n]; b > 0 && g.Usage.DayCalls(n) >= b {
			last = errorf(http.StatusTooManyRequests, "provider_budget", "%s has used its %d calls for today (change with berth tools:key %s --daily ...)", n, b, n)
			continue
		}
		began := time.Now()
		limit := g.Timeout
		if t.Timeout > 0 {
			limit = t.Timeout
		}
		cctx, cancel := context.WithTimeout(ctx, limit)
		out, err := t.Run[n](cctx, g.newCall(app.Name, keys[n]), args)
		cancel()
		spent = true
		var final *Error
		if errors.As(err, &final) {
			meta.Provider = n
			record(n, false, final.Status, began)
			return nil, meta, final
		}
		if err != nil {
			slog.Warn("tool provider failed", "tool", t.Name, "provider", n, "err", err)
			record(n, false, http.StatusBadGateway, began)
			last = fmt.Errorf("%s %w", n, err)
			continue
		}
		if p, ok := out.(Pending); ok {
			j, err := g.jobs.create(app.Name, t.Name, n, p.Op)
			if err != nil {
				last = err
				continue
			}
			meta.Provider, meta.Job = n, true
			record(n, false, http.StatusAccepted, began)
			b, _ := json.Marshal(j.view())
			return b, meta, nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			last = err
			continue
		}
		meta.Provider = n
		record(n, false, http.StatusOK, began)
		if t.Cache > 0 && force == "" {
			g.cache.put(key, cached{b, n, time.Now().Add(t.Cache)})
		}
		return b, meta, nil
	}
	var final *Error
	if errors.As(last, &final) {
		if spent {
			return nil, meta, final
		}
		return refuse(final)
	}
	return nil, meta, errorf(http.StatusBadGateway, "provider_failed", "%s failed: %s", t.Name, last)
}

func (g *Gateway) admitRate(app App) *Error {
	rpm := app.Policy.RPM
	if rpm <= 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	recent := g.rate[app.Name][:0]
	for _, at := range g.rate[app.Name] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= rpm {
		g.rate[app.Name] = recent
		return errorf(http.StatusTooManyRequests, "rate_limit", "%s is limited to %d calls a minute (change with berth tools:enable %s --rpm ...)", app.Name, rpm, app.Name)
	}
	g.rate[app.Name] = append(recent, now)
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *Error) {
	writeJSON(w, e.Status, map[string]any{"error": e})
}
