package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dps/berth/internal/ai"
	"github.com/dps/berth/internal/state"
)

// ---- AI gateway (design.md, AI gateway) ----

// Env vars ai:enable sets. Every OpenAI and Anthropic SDK reads them, so
// apps need no Berth-specific code.
const (
	envOpenAIBase    = "OPENAI_BASE_URL"
	envOpenAIKey     = "OPENAI_API_KEY"
	envAnthropicBase = "ANTHROPIC_BASE_URL"
	envAnthropicKey  = "ANTHROPIC_API_KEY"
)

func (d *Daemon) startAI(cfg *state.Config) (*http.Server, error) {
	d.ai = ai.New(ai.OpenUsage(d.Store.Path("ai")))
	ln, err := net.Listen("tcp", cfg.AIAddr)
	if err != nil {
		return nil, fmt.Errorf("AI gateway listener %s: %w", cfg.AIAddr, err)
	}
	// No write timeout: responses stream for as long as the model writes.
	srv := &http.Server{Handler: d.ai, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return srv, nil
}

func (d *Daemon) aiKeysPath() string { return filepath.Join(d.Store.SecretsDir(), "ai.json") }

func (d *Daemon) aiKeys() map[string]string {
	keys := map[string]string{}
	if b, err := os.ReadFile(d.aiKeysPath()); err == nil {
		json.Unmarshal(b, &keys)
	}
	return keys
}

func (d *Daemon) saveAIKeys(keys map[string]string) error {
	b, _ := json.MarshalIndent(keys, "", "  ")
	return state.WriteFile(d.aiKeysPath(), append(b, '\n'), 0o600)
}

// configureAI loads the gateway's setup and returns where Caddy should
// listen for it ("" until AI is set up or an app uses it, or the tools
// gateway, which shares the port, is on).
func (d *Daemon) configureAI(cfg *state.Config, apps []*state.App, toolsOn bool) (listen string, sources []string) {
	var gwApps []ai.App
	for _, a := range apps {
		if a.AI != nil {
			gwApps = append(gwApps, ai.App{Name: a.Name, Policy: *a.AI})
		}
	}
	if d.tools != nil {
		// Tools like translation can use the models apps use.
		gwApps = append(gwApps, ai.App{Name: "@tools", Policy: state.AppAI{TokenHash: ai.HashToken(d.tools.ai)}})
	}
	if d.ai != nil {
		ac, err := d.Store.AIConfig()
		if err != nil {
			slog.Warn("ai config", "err", err)
			ac = &state.AIConfig{}
		}
		d.ai.Configure(ac, d.aiKeys(), gwApps)
	}
	if !d.Store.AIConfigured() && !toolsOn && !anyAI(apps) || cfg.AIPort == 0 {
		return "", nil
	}
	sources = []string{"127.0.0.1/32", "::1/128"}
	if ip := net.ParseIP(cfg.Gateway).To4(); ip != nil {
		sources = append(sources, (&net.IPNet{IP: ip.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}).String())
	}
	return fmt.Sprintf(":%d", cfg.AIPort), sources
}

// aiBase is the gateway's URL as apps see it.
func aiBase(cfg *state.Config) string {
	return fmt.Sprintf("http://%s:%d", cfg.Gateway, cfg.AIPort)
}

// aiEnv is what ai:enable puts in an app's env.
func aiEnv(cfg *state.Config, token string) map[string]string {
	base := aiBase(cfg)
	return map[string]string{
		envOpenAIBase: base + "/v1", envOpenAIKey: token,
		envAnthropicBase: base + "/anthropic", envAnthropicKey: token,
	}
}

type AISetupParams struct {
	Model string `json:"model"` // local model for the "default" alias
}

// hAISetup creates the gateway's config, with "default" pointing at the
// local model unless it is already set. The CLI installs Ollama and pulls
// the model first.
func (d *Daemon) hAISetup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AISetupParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg := d.cfg()
	if !d.Store.AIConfigured() {
		// Caddy will listen here; say so now rather than fail its reload.
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.AIPort))
		if err != nil {
			return nil, fmt.Errorf("port %d is taken (set ai_port in ~/.berth/config.json, then berth reload): %w", cfg.AIPort, err)
		}
		ln.Close()
	}
	ac, err := d.Store.AIConfig()
	if err != nil {
		return nil, err
	}
	if p.Model != "" {
		ref := ai.Local + "/" + p.Model
		if err := state.ValidateAIModel(ref); err != nil {
			return nil, err
		}
		if _, ok := ac.Aliases["default"]; !ok {
			ac.Aliases["default"] = ref
			fmt.Fprintf(out, "-----> Alias default → %s\n", ref)
		}
	}
	if err := d.Store.SaveAIConfig(ac); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> AI gateway ready; apps reach it at %s once enabled (berth ai:enable <app>)\n", aiBase(cfg))
	return map[string]string{"url": aiBase(cfg)}, nil
}

type AIKeyParams struct {
	Provider string `json:"provider"`
	Key      string `json:"key,omitempty"`
	URL      string `json:"url,omitempty"` // OpenAI-compatible base, for providers Berth doesn't know
	Remove   bool   `json:"remove,omitempty"`
}

func (d *Daemon) hAIKey(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AIKeyParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := state.ValidateAIName("provider", p.Provider); err != nil {
		return nil, err
	}
	if p.Provider == ai.Local {
		return nil, fmt.Errorf("local models need no key (set ollama_url in ~/.berth/ai/config.json to use another Ollama)")
	}
	ac, err := d.Store.AIConfig()
	if err != nil {
		return nil, err
	}
	keys := d.aiKeys()
	_, builtin := ai.Builtin[p.Provider]
	if p.Remove {
		delete(keys, p.Provider)
		delete(ac.Providers, p.Provider)
		fmt.Fprintf(out, "-----> Removed %s\n", p.Provider)
	} else {
		if p.URL != "" {
			if builtin {
				return nil, fmt.Errorf("%s is built in; --url is for other OpenAI-compatible providers", p.Provider)
			}
			if !strings.HasPrefix(p.URL, "https://") && !strings.HasPrefix(p.URL, "http://") {
				return nil, fmt.Errorf("--url must start with https:// (or http:// for a server on your network)")
			}
			ac.Providers[p.Provider] = state.AIProvider{URL: strings.TrimSuffix(p.URL, "/")}
		} else if _, custom := ac.Providers[p.Provider]; !builtin && !custom {
			known := make([]string, 0, len(ai.Builtin))
			for k := range ai.Builtin {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("unknown provider %q (built in: %s); for any OpenAI-compatible service add --url", p.Provider, strings.Join(known, ", "))
		}
		if p.Key == "" && p.URL == "" {
			return nil, fmt.Errorf("no key given")
		}
		if p.Key != "" {
			keys[p.Provider] = p.Key
		}
		fmt.Fprintf(out, "-----> Saved %s (apps never see this key; they use their own Berth token)\n", p.Provider)
	}
	if err := d.saveAIKeys(keys); err != nil {
		return nil, err
	}
	if err := d.Store.SaveAIConfig(ac); err != nil {
		return nil, err
	}
	return map[string]string{"provider": p.Provider}, d.Apply(ctx)
}

type AIAliasParams struct {
	Name   string `json:"name"`
	Model  string `json:"model,omitempty"`
	Remove bool   `json:"remove,omitempty"`
}

func (d *Daemon) hAIAlias(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AIAliasParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := state.ValidateAIName("alias", p.Name); err != nil {
		return nil, err
	}
	ac, err := d.Store.AIConfig()
	if err != nil {
		return nil, err
	}
	if p.Remove {
		delete(ac.Aliases, p.Name)
		fmt.Fprintf(out, "-----> Removed alias %s\n", p.Name)
	} else {
		if err := state.ValidateAIModel(p.Model); err != nil {
			return nil, err
		}
		if _, isAlias := ac.Aliases[p.Model]; isAlias {
			return nil, fmt.Errorf("aliases point at provider/model, not at other aliases")
		}
		if _, err := ai.Resolve(&state.AIConfig{Providers: ac.Providers}, p.Model); err != nil {
			return nil, err
		}
		ac.Aliases[p.Name] = p.Model
		fmt.Fprintf(out, "-----> %s → %s (apps asking for %q switch now; nothing redeploys)\n", p.Name, p.Model, p.Name)
	}
	if err := d.Store.SaveAIConfig(ac); err != nil {
		return nil, err
	}
	return map[string]string{"alias": p.Name, "model": p.Model}, d.Apply(ctx)
}

type AIEnableParams struct {
	App           string   `json:"app"`
	Models        []string `json:"models,omitempty"`
	MonthlyTokens int64    `json:"monthly_tokens,omitempty"`
	RPM           int      `json:"rpm,omitempty"`
	Replace       bool     `json:"replace,omitempty"` // overwrite the app's own OPENAI_/ANTHROPIC_ variables
	NoRestart     bool     `json:"no_restart,omitempty"`
}

// hAIEnable gives an app a token and points its SDK variables at the
// gateway. Re-running it updates the limits and keeps the token.
func (d *Daemon) hAIEnable(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AIEnableParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	for _, m := range p.Models {
		var err error
		switch provider, ok := strings.CutSuffix(m, "/*"); {
		case ok:
			err = state.ValidateAIName("provider", provider)
		case strings.Contains(m, "/"):
			err = state.ValidateAIModel(m)
		default:
			err = state.ValidateAIName("alias", m)
		}
		if err != nil {
			return nil, err
		}
	}
	if !d.Store.AIConfigured() {
		return nil, fmt.Errorf("the AI gateway isn't set up yet (berth ai:setup)")
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
	if !p.Replace {
		for _, k := range []string{envOpenAIKey, envAnthropicKey} {
			if v, ok := env[k]; ok && !strings.HasPrefix(v, ai.TokenPrefix) {
				unlock()
				return nil, fmt.Errorf("%s already has its own %s; --replace swaps it for the gateway (keep the key with `berth ai:key`)", p.App, k)
			}
		}
	}
	token := env[envOpenAIKey]
	if a.AI == nil || ai.HashToken(token) != a.AI.TokenHash {
		var hash string
		token, hash = ai.NewToken()
		a.AI = &state.AppAI{TokenHash: hash}
	}
	a.AI.Models, a.AI.MonthlyTokens, a.AI.RPM = p.Models, p.MonthlyTokens, p.RPM
	for k, v := range aiEnv(d.cfg(), token) {
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
	fmt.Fprintf(out, "-----> %s can use the AI gateway: %s, %s and %s set\n", p.App, envOpenAIBase, envOpenAIKey, envAnthropicBase)
	fmt.Fprintf(out, "       models: %s; tokens a month: %s; requests a minute: %s\n", orAll(strings.Join(p.Models, ", ")), orAll(count(p.MonthlyTokens)), orAll(count(int64(p.RPM))))
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

func anyAI(apps []*state.App) bool {
	for _, a := range apps {
		if a.AI != nil {
			return true
		}
	}
	return false
}

func orAll(s string) string {
	if s == "" || s == "0" {
		return "any"
	}
	return s
}

func count(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

func (d *Daemon) hAIDisable(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
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
	base := aiBase(d.cfg())
	for k, v := range env {
		if strings.HasPrefix(v, ai.TokenPrefix) || strings.HasPrefix(v, base) {
			delete(env, k)
		}
	}
	a.AI = nil
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
	fmt.Fprintf(out, "-----> %s's AI token is revoked\n", p.App)
	return d.redeployForConfig(ctx, p.App, false, false, out)
}

type AIProviderStatus struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	HasKey bool   `json:"has_key"`
}

type AIAppStatus struct {
	App           string   `json:"app"`
	Models        []string `json:"models,omitempty"`
	MonthlyTokens int64    `json:"monthly_tokens,omitempty"`
	RPM           int      `json:"rpm,omitempty"`
	UsedTokens    int64    `json:"used_tokens"`
}

type AIStatus struct {
	Configured    bool               `json:"configured"`
	URL           string             `json:"url"`
	OllamaURL     string             `json:"ollama_url"`
	OllamaVersion string             `json:"ollama_version,omitempty"`
	OllamaError   string             `json:"ollama_error,omitempty"`
	Local         []ai.LocalModel    `json:"local,omitempty"`
	Loaded        []ai.LoadedModel   `json:"loaded,omitempty"`
	Aliases       map[string]string  `json:"aliases"`
	Providers     []AIProviderStatus `json:"providers"`
	Apps          []AIAppStatus      `json:"apps"`
}

func (d *Daemon) aiStatus(ctx context.Context) (*AIStatus, error) {
	ac, err := d.Store.AIConfig()
	if err != nil {
		return nil, err
	}
	keys := d.aiKeys()
	st := &AIStatus{Configured: d.Store.AIConfigured(), URL: aiBase(d.cfg()), Aliases: ac.Aliases, OllamaURL: strings.TrimSuffix(ai.BaseURL(ac, ai.Local), "/v1")}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if v, err := ai.OllamaVersion(ctx, st.OllamaURL); err != nil {
		st.OllamaError = err.Error()
	} else {
		st.OllamaVersion = v
		st.Local, _ = ai.OllamaModels(ctx, st.OllamaURL)
		st.Loaded, _ = ai.OllamaLoaded(ctx, st.OllamaURL)
	}
	names := map[string]bool{}
	for n := range ai.Builtin {
		names[n] = true
	}
	for n := range ac.Providers {
		names[n] = true
	}
	for n := range names {
		_, hasKey := keys[n]
		_, custom := ac.Providers[n]
		if hasKey || custom {
			st.Providers = append(st.Providers, AIProviderStatus{Name: n, URL: ai.BaseURL(ac, n), HasKey: hasKey})
		}
	}
	sort.Slice(st.Providers, func(i, j int) bool { return st.Providers[i].Name < st.Providers[j].Name })
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		if a.AI == nil {
			continue
		}
		used := int64(0)
		if d.ai != nil {
			used = d.ai.Usage.MonthTokens(a.Name)
		}
		st.Apps = append(st.Apps, AIAppStatus{App: a.Name, Models: a.AI.Models, MonthlyTokens: a.AI.MonthlyTokens, RPM: a.AI.RPM, UsedTokens: used})
	}
	return st, nil
}

func (d *Daemon) hAIStatus(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	return d.aiStatus(ctx)
}

type AIUsageParams struct {
	App   string `json:"app,omitempty"`
	Month string `json:"month,omitempty"` // 2026-10; default this month
}

func (d *Daemon) hAIUsage(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AIUsageParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Month == "" {
		p.Month = time.Now().Format("2006-01")
	} else if _, err := time.Parse("2006-01", p.Month); err != nil {
		return nil, fmt.Errorf("--month: use YYYY-MM")
	}
	if d.ai == nil {
		return []ai.Row{}, nil
	}
	return d.ai.Usage.Summary(p.Month, p.App), nil
}

// devAI gives a dev version its own gateway token, limited to local models
// so an agent iterating on it can't run up a cloud bill. env is the copy of
// the parent's; its token is replaced.
func devAI(cfg *state.Config, parent *state.App, dev *state.App, env state.Env) {
	if parent.AI == nil {
		return
	}
	token, hash := ai.NewToken()
	dev.AI = &state.AppAI{TokenHash: hash, Models: []string{ai.Local + "/*"}, MonthlyTokens: parent.AI.MonthlyTokens, RPM: parent.AI.RPM}
	for k, v := range aiEnv(cfg, token) {
		env[k] = v
	}
}

// usesLocal reports whether any alias points at a local model.
func usesLocal(aliases map[string]string) bool {
	for _, to := range aliases {
		if strings.HasPrefix(to, ai.Local+"/") {
			return true
		}
	}
	return false
}
