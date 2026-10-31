package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dps/berth/internal/ai"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tools"
)

// ---- Tools gateway (design.md, Tools gateway) ----

// Env vars tools:enable sets.
const (
	envToolsURL = "BERTH_TOOLS_URL"
	envToolsKey = "BERTH_TOOLS_KEY"
)

// toolsChild runs `berthd tools` while the gateway is in use.
type toolsChild struct {
	exe   string
	admin string // token for tools:call, new each time berthd starts
	ai    string // its token for the AI gateway, likewise
	want  chan bool

	mu   sync.Mutex
	proc *os.Process
}

func (c *toolsChild) setProc(p *os.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proc = p
}

// set says whether the child should run, and tells a running one to
// re-read its setup.
func (c *toolsChild) set(on bool) {
	select {
	case <-c.want:
	default:
	}
	c.want <- on
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proc != nil {
		c.proc.Signal(syscall.SIGHUP)
	}
}

func (d *Daemon) startTools(ctx context.Context) {
	exe, err := os.Executable()
	if err != nil {
		slog.Error("tools gateway", "err", err)
		return
	}
	d.tools = &toolsChild{exe: exe, admin: randomHex(24), want: make(chan bool, 1)}
	d.tools.ai, _ = ai.NewToken()
	go d.tools.supervise(ctx)
	apps, _ := d.Store.Apps()
	d.configureTools(apps)
}

// supervise starts the child when it's wanted, and again if it dies,
// waiting longer each time it dies quickly.
func (c *toolsChild) supervise(ctx context.Context) {
	wanted := false
	delay := time.Second
	for {
		for !wanted {
			select {
			case wanted = <-c.want:
			case <-ctx.Done():
				return
			}
		}
		cmd := exec.CommandContext(ctx, c.exe, "tools")
		cmd.Env = append(os.Environ(), tools.AdminEnv+"="+c.admin, tools.AIEnv+"="+c.ai)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		// The child exits when this pipe closes, so it can't outlive
		// berthd, even across a crash or a re-exec.
		stdin, err := cmd.StdinPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			slog.Error("tools gateway", "err", err)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
			continue
		}
		c.setProc(cmd.Process)
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
	running:
		for {
			select {
			case wanted = <-c.want:
				if !wanted {
					stdin.Close()
				}
			case err := <-done:
				c.setProc(nil)
				if ctx.Err() != nil {
					return
				}
				if !wanted {
					break running
				}
				slog.Warn("tools gateway exited; restarting", "err", err)
				if time.Since(started) > time.Minute {
					delay = time.Second
				}
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
				delay = min(delay*2, 30*time.Second)
				break running
			}
		}
		stdin.Close()
	}
}

// configureTools starts, stops or reloads the child to match the current
// setup, and reports whether Caddy should route to it.
func (d *Daemon) configureTools(apps []*state.App) bool {
	on := d.Store.ToolsConfigured()
	for _, a := range apps {
		on = on || a.Tools != nil
	}
	if d.tools != nil {
		d.tools.set(on)
	}
	return on
}

// toolKeys is every provider key tools can use. Only whether "ai" is set
// matters here; the child has the token.
func (d *Daemon) toolKeys() map[string]string { return tools.Keys(d.Store, "set") }

// toolsBase is the gateway's URL as apps see it. It shares the AI gateway's
// port, so a Berth using both opens one.
func toolsBase(cfg *state.Config) string { return aiBase(cfg) + "/tools" }

func toolsEnv(cfg *state.Config, token string) map[string]string {
	return map[string]string{envToolsURL: toolsBase(cfg), envToolsKey: token}
}

// prepareTools creates the gateway's config the first time, checking the
// port Caddy will need.
func (d *Daemon) prepareTools(ctx context.Context) error {
	if d.Store.ToolsConfigured() {
		return nil
	}
	cfg := d.cfg()
	if !d.Store.AIConfigured() {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.AIPort))
		if err != nil {
			return fmt.Errorf("port %d is taken (set ai_port in ~/.berth/config.json, then berth reload): %w", cfg.AIPort, err)
		}
		ln.Close()
	}
	return d.Store.SaveToolsConfig(&state.ToolsConfig{})
}

type ToolsKeyParams struct {
	Provider string `json:"provider"`
	Key      string `json:"key,omitempty"`
	Daily    *int   `json:"daily,omitempty"` // calls a day across every app; 0 clears it
	Remove   bool   `json:"remove,omitempty"`
}

func (d *Daemon) hToolsKey(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsKeyParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	prov, ok := tools.FindProvider(p.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (key providers: %s)", p.Provider, strings.Join(tools.ProviderNames(), ", "))
	}
	if (prov.Keyless() || prov.Internal) && p.Key != "" {
		return nil, fmt.Errorf("%s needs no key", p.Provider)
	}
	if prov.KeyHint != "" && p.Key != "" && strings.Count(p.Key, ":") != strings.Count(prov.KeyHint, ":") {
		return nil, fmt.Errorf("the %s key has the form %s", p.Provider, prov.KeyHint)
	}
	if err := d.prepareTools(ctx); err != nil {
		return nil, err
	}
	cfg, err := d.Store.ToolsConfig()
	if err != nil {
		return nil, err
	}
	keys := d.Store.ToolKeys()
	switch {
	case p.Remove:
		delete(keys, p.Provider)
		delete(cfg.Budgets, p.Provider)
		fmt.Fprintf(out, "-----> Removed %s\n", p.Provider)
	case p.Key != "":
		keys[p.Provider] = p.Key
		fmt.Fprintf(out, "-----> Saved %s (apps never see this key; they use their own Berth token)\n", p.Provider)
	case p.Daily == nil:
		return nil, fmt.Errorf("no key given")
	}
	if p.Daily != nil && !p.Remove {
		if *p.Daily < 0 {
			return nil, fmt.Errorf("--daily can't be negative")
		}
		if *p.Daily == 0 {
			delete(cfg.Budgets, p.Provider)
			fmt.Fprintf(out, "-----> %s has no daily budget\n", p.Provider)
		} else {
			cfg.Budgets[p.Provider] = *p.Daily
			fmt.Fprintf(out, "-----> %s is limited to %d calls a day across all apps\n", p.Provider, *p.Daily)
		}
	}
	if err := d.Store.SaveToolKeys(keys); err != nil {
		return nil, err
	}
	if err := d.Store.SaveToolsConfig(cfg); err != nil {
		return nil, err
	}
	return map[string]string{"provider": p.Provider}, d.Apply(ctx)
}

type ToolsUseParams struct {
	Tool      string   `json:"tool"`
	Providers []string `json:"providers,omitempty"` // empty goes back to the built-in order
}

// hToolsUse picks the providers behind a tool. Apps switch on their next
// call; nothing redeploys.
func (d *Daemon) hToolsUse(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsUseParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	t := tools.Find(p.Tool)
	if t == nil {
		return nil, fmt.Errorf("no tool %s (see berth tools)", p.Tool)
	}
	keys := d.toolKeys()
	for _, n := range p.Providers {
		if _, ok := t.Run[n]; !ok {
			return nil, fmt.Errorf("%s can use %s, not %s", p.Tool, strings.Join(t.Order, ", "), n)
		}
		if prov, _ := tools.FindProvider(n); !prov.Keyless() && !prov.Internal && keys[n] == "" {
			fmt.Fprintf(out, "       %s has no key yet (berth tools:key %s)\n", n, n)
		}
	}
	if err := d.prepareTools(ctx); err != nil {
		return nil, err
	}
	cfg, err := d.Store.ToolsConfig()
	if err != nil {
		return nil, err
	}
	if len(p.Providers) == 0 {
		delete(cfg.Routes, p.Tool)
		fmt.Fprintf(out, "-----> %s uses its default order: %s\n", p.Tool, strings.Join(t.Order, ", "))
	} else {
		cfg.Routes[p.Tool] = p.Providers
		fmt.Fprintf(out, "-----> %s → %s (apps switch now; nothing redeploys)\n", p.Tool, strings.Join(p.Providers, ", "))
	}
	if err := d.Store.SaveToolsConfig(cfg); err != nil {
		return nil, err
	}
	return map[string]any{"tool": p.Tool, "providers": p.Providers}, d.Apply(ctx)
}

type ToolsEnableParams struct {
	App          string   `json:"app"`
	Tools        []string `json:"tools,omitempty"`
	RPM          int      `json:"rpm,omitempty"`
	MonthlyCalls int64    `json:"monthly_calls,omitempty"`
	Full         bool     `json:"full,omitempty"` // a dev version may use keyed providers
	NoRestart    bool     `json:"no_restart,omitempty"`
}

// hToolsEnable gives an app a token and the gateway's URL. Re-running it
// updates the limits and keeps the token.
func (d *Daemon) hToolsEnable(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsEnableParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	for _, pat := range p.Tools {
		if err := tools.ValidPattern(pat); err != nil {
			return nil, err
		}
	}
	if err := d.prepareTools(ctx); err != nil {
		return nil, err
	}
	unlock := d.Deployer.Lock(p.App)
	a, err := d.Store.App(p.App)
	if err != nil {
		unlock()
		return nil, err
	}
	env, err := d.Store.Env(p.App)
	if err != nil {
		unlock()
		return nil, err
	}
	token := env[envToolsKey]
	if a.Tools == nil || tools.HashToken(token) != a.Tools.TokenHash {
		var hash string
		token, hash = tools.NewToken()
		a.Tools = &state.AppTools{TokenHash: hash}
	}
	a.Tools.Tools, a.Tools.RPM, a.Tools.MonthlyCalls = p.Tools, p.RPM, p.MonthlyCalls
	a.Tools.KeylessOnly = a.DevOf != "" && !p.Full
	vars := toolsEnv(d.cfg(), token)
	changed := false
	for k, v := range vars {
		changed = changed || env[k] != v
		env[k] = v
	}
	err = d.Store.SaveEnv(p.App, env)
	if err == nil {
		err = d.Store.SaveApp(a)
	}
	unlock()
	if err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> %s can use tools: %s and %s set\n", p.App, envToolsURL, envToolsKey)
	fmt.Fprintf(out, "       tools: %s; calls a month: %s; calls a minute: %s\n", orAll(strings.Join(p.Tools, ", ")), orAll(count(p.MonthlyCalls)), orAll(count(int64(p.RPM))))
	if !changed {
		return map[string]bool{"restarted": false}, nil // limits apply at once
	}
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

type ToolsDisableParams struct {
	App       string `json:"app"`
	NoRestart bool   `json:"no_restart,omitempty"`
}

func (d *Daemon) hToolsDisable(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsDisableParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	unlock := d.Deployer.Lock(p.App)
	a, err := d.Store.App(p.App)
	if err != nil {
		unlock()
		return nil, err
	}
	env, _ := d.Store.Env(p.App)
	delete(env, envToolsURL)
	delete(env, envToolsKey)
	a.Tools = nil
	err = d.Store.SaveEnv(p.App, env)
	if err == nil {
		err = d.Store.SaveApp(a)
	}
	unlock()
	if err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> %s's tools token is revoked\n", p.App)
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

type ToolStatus struct {
	Name      string   `json:"name"`
	Summary   string   `json:"summary"`
	Costly    bool     `json:"costly,omitempty"`
	Providers []string `json:"providers"` // ready now, first choice first
	Order     []string `json:"order"`     // every provider, first choice first
	Custom    bool     `json:"custom,omitempty"`
	Needs     string   `json:"needs,omitempty"`
}

type ToolsProviderStatus struct {
	Name   string   `json:"name"`
	Title  string   `json:"title"`
	SignUp string   `json:"signup"`
	Hint   string   `json:"hint,omitempty"`
	Note   string   `json:"note,omitempty"`
	HasKey bool     `json:"has_key"`
	Shared bool     `json:"shared,omitempty"` // the key is the AI gateway's
	Budget int      `json:"budget,omitempty"`
	Today  int      `json:"today"`
	Tools  []string `json:"tools"`
	Test   bool     `json:"test"` // can be tested with a sample call
}

type ToolsAppStatus struct {
	App          string   `json:"app"`
	Tools        []string `json:"tools,omitempty"`
	RPM          int      `json:"rpm,omitempty"`
	MonthlyCalls int64    `json:"monthly_calls,omitempty"`
	KeylessOnly  bool     `json:"keyless_only,omitempty"`
	Dev          bool     `json:"dev,omitempty"`
	Used         int64    `json:"used"`
}

type ToolsStatus struct {
	Configured bool                  `json:"configured"`
	URL        string                `json:"url"`
	Tools      []ToolStatus          `json:"tools"`
	Providers  []ToolsProviderStatus `json:"providers"`
	Apps       []ToolsAppStatus      `json:"apps"`
	Others     []string              `json:"others"` // apps without access
}

func (d *Daemon) hToolsStatus(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	cfg, err := d.Store.ToolsConfig()
	if err != nil {
		return nil, err
	}
	keys := d.toolKeys()
	usage := tools.OpenUsage(d.Store.Path("tools"))
	st := &ToolsStatus{Configured: d.Store.ToolsConfigured(), URL: toolsBase(d.cfg())}
	for _, t := range tools.Catalog() {
		ts := ToolStatus{Name: t.Name, Summary: t.Summary, Costly: t.Costly, Providers: tools.Route(t, cfg.Routes, keys), Order: t.Order}
		if r := cfg.Routes[t.Name]; len(r) > 0 {
			ts.Order, ts.Custom = r, true
		}
		if len(ts.Providers) == 0 {
			ts.Needs = "berth tools:key " + t.Order[0]
		}
		st.Tools = append(st.Tools, ts)
	}
	for _, prov := range tools.AllProviders() {
		n := prov.Name
		_, sample := tools.Sample(n)
		st.Providers = append(st.Providers, ToolsProviderStatus{Name: n, Title: prov.Title, SignUp: prov.SignUp, Hint: prov.KeyHint, Note: prov.Note,
			HasKey: keys[n] != "", Shared: tools.Shared(d.Store, n), Budget: cfg.Budgets[n], Today: usage.DayCalls(n), Tools: tools.ToolsOf(n), Test: sample})
	}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		if a.Tools != nil {
			st.Apps = append(st.Apps, ToolsAppStatus{App: a.Name, Tools: a.Tools.Tools, RPM: a.Tools.RPM, MonthlyCalls: a.Tools.MonthlyCalls,
				KeylessOnly: a.Tools.KeylessOnly, Dev: a.DevOf != "", Used: usage.MonthCalls(a.Name)})
		} else {
			st.Others = append(st.Others, a.Name)
		}
	}
	return st, nil
}

type ToolsUsageParams struct {
	App   string `json:"app,omitempty"`
	Month string `json:"month,omitempty"` // 2026-10; default this month
}

func (d *Daemon) hToolsUsage(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsUsageParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Month == "" {
		p.Month = time.Now().Format("2006-01")
	} else if _, err := time.Parse("2006-01", p.Month); err != nil {
		return nil, fmt.Errorf("--month: use YYYY-MM")
	}
	return tools.OpenUsage(d.Store.Path("tools")).Summary(p.Month, p.App), nil
}

type ToolsDocsParams struct {
	App string `json:"app,omitempty"` // only what this app may use; empty is every tool
}

// hToolsDocs renders the tools as Markdown for a coding agent.
func (d *Daemon) hToolsDocs(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsDocsParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg, err := d.Store.ToolsConfig()
	if err != nil {
		return nil, err
	}
	app := tools.Everyone()
	if p.App != "" {
		a, err := d.Store.App(p.App)
		if err != nil {
			return nil, err
		}
		if a.Tools == nil {
			return nil, fmt.Errorf("%s doesn't use tools yet (berth tools:enable %s)", p.App, p.App)
		}
		app = tools.App{Policy: *a.Tools}
	}
	return map[string]string{"markdown": tools.Docs(tools.Visible(app, cfg.Routes, d.toolKeys()))}, nil
}

type ToolsCallParams struct {
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args,omitempty"`
	Provider string          `json:"provider,omitempty"` // use only this one, skipping the cache
}

// hToolsCall runs a tool as the owner, through the same gateway apps use.
func (d *Daemon) hToolsCall(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ToolsCallParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	t := tools.Find(p.Tool)
	if t == nil {
		return nil, fmt.Errorf("no tool %s (see berth tools)", p.Tool)
	}
	if d.tools == nil {
		return nil, errors.New("the tools gateway isn't running")
	}
	if !d.Store.ToolsConfigured() {
		if err := d.prepareTools(ctx); err != nil {
			return nil, err
		}
		if err := d.Apply(ctx); err != nil {
			return nil, err
		}
	}
	url := "http://" + d.cfg().ToolsAddr + t.Path()
	// A child that was just started needs a moment to listen.
	var resp *http.Response
	var err error
	for range 25 {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(p.Args))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Authorization", "Bearer "+d.tools.admin)
		if p.Provider != "" {
			req.Header.Set(tools.ProviderHeader, p.Provider)
		}
		if resp, err = http.DefaultClient.Do(req); err == nil || !errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("the tools gateway isn't answering: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Result json.RawMessage `json:"result"`
		Job    json.RawMessage `json:"job"`
		Meta   tools.Meta      `json:"meta"`
		Error  *tools.Error    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("the tools gateway answered %d", resp.StatusCode)
	}
	if body.Error != nil {
		return nil, errors.New(body.Error.Message)
	}
	if body.Meta.Job {
		body.Result = body.Job
	}
	return map[string]any{"result": body.Result, "meta": body.Meta}, nil
}

// devTools gives a dev version its own token, limited to keyless providers
// and the cache so an agent iterating on it can't spend the owner's quota.
// env is the copy of the parent's; its token is replaced.
func devTools(cfg *state.Config, parent *state.App, dev *state.App, env state.Env) {
	if parent.Tools == nil {
		return
	}
	token, hash := tools.NewToken()
	dev.Tools = &state.AppTools{TokenHash: hash, Tools: parent.Tools.Tools, RPM: parent.Tools.RPM, MonthlyCalls: parent.Tools.MonthlyCalls, KeylessOnly: true}
	for k, v := range toolsEnv(cfg, token) {
		env[k] = v
	}
}
