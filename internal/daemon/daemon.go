// Package daemon is berthd: the only writer of ~/.berth. It serves the CLI
// over a unix socket, receives GitHub webhooks, runs deploys, keeps
// containers alive, tails logs, polls branches and runs nightly backups.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dps/berth/internal/caddy"
	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/gitx"
	"github.com/dps/berth/internal/launchd"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/services"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

var Version = "dev"

type Daemon struct {
	Store    *state.Store
	RT       runtime.Runtime
	Deployer *deploy.Deployer
	Services *services.Manager
	Caddy    *caddy.Client
	// GitHubAPI and CloudflareAPI build API clients; overridable in tests.
	GitHubAPI     func(token string) *github.Client
	CloudflareAPI func(token string) *tunnel.API

	// Intervals (tests shorten them).
	SuperviseEvery time.Duration
	PollEvery      time.Duration

	started time.Time
	exe     string
	exeMod  time.Time

	mu            sync.Mutex
	crash         map[string]*crashState
	tails         map[string]context.CancelFunc // container -> tail cancel
	queued        map[string]bool
	hookHits      map[string][]time.Time // rate limiting per app
	mdns          *mdnsRegistry
	restoring     atomic.Bool // pauses the supervisor while `berth restore` rebuilds state
	cfMu          sync.Mutex
	lastTunnelCfg string

	ctx    context.Context
	cancel context.CancelFunc
}

func New(store *state.Store, rt runtime.Runtime) *Daemon {
	d := &Daemon{
		Store:          store,
		RT:             rt,
		Services:       &services.Manager{Store: store, RT: rt},
		GitHubAPI:      github.New,
		CloudflareAPI:  tunnel.NewAPI,
		SuperviseEvery: 10 * time.Second,
		PollEvery:      60 * time.Second,
		crash:          map[string]*crashState{},
		tails:          map[string]context.CancelFunc{},
		queued:         map[string]bool{},
		hookHits:       map[string][]time.Time{},
		mdns:           newMDNS(),
	}
	d.Deployer = deploy.New(store, rt, d)
	d.Deployer.GitAuth = d.gitAuth
	d.Deployer.Event = d.event
	return d
}

func (d *Daemon) cfg() *state.Config {
	c, err := d.Store.Config()
	if err != nil {
		return state.DefaultConfig()
	}
	return c
}

// ---- secrets ----

func (d *Daemon) secret(name string) string {
	b, err := os.ReadFile(filepath.Join(d.Store.SecretsDir(), name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (d *Daemon) githubToken() string { return d.secret("github.token") }
func (d *Daemon) cfToken() string     { return d.secret("cloudflare.token") }

func (d *Daemon) gitAuth(a *state.App) gitx.Auth {
	auth := gitx.Auth{KnownHosts: filepath.Join(d.Store.SecretsDir(), "known_hosts")}
	if a.DeployKey {
		auth.SSHKey = filepath.Join(d.Store.SecretsDir(), "keys", a.Name)
	} else if a.IsGitHub() {
		auth.Token = d.githubToken()
	}
	return auth
}

// ---- events and notifications ----

func (d *Daemon) event(ev state.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if err := d.Store.AppendEvent(ev); err != nil {
		slog.Warn("event log", "err", err)
	}
	slog.Info(ev.Message, "kind", ev.Kind, "app", ev.App)
	switch ev.Kind {
	case "deploy.ok", "deploy.failed", "crash.loop", "backup.failed":
		go d.notify(ev)
	}
}

// ---- routing (deploy.Router) ----

// Apply rebuilds the Caddy config from state and loads it.
func (d *Daemon) Apply(ctx context.Context) error {
	cfg := d.cfg()
	apps, err := d.Store.Apps()
	if err != nil {
		return err
	}
	var routes []caddy.Route
	for _, a := range apps {
		r := caddy.Route{App: a.Name, AccessLog: d.Store.Path("apps", a.Name, "logs", "access.log")}
		if !a.Private && !cfg.NoTunnel && cfg.Domain != "" {
			r.PublicHosts = append([]string{a.Name + "." + cfg.Domain}, a.Domains...)
		}
		if cfg.Hostname != "" {
			r.LANHosts = []string{a.Name + "." + cfg.Hostname + ".local"}
		}
		if cfg.Tailscale && cfg.TailnetDomain != "" {
			r.LANHosts = append(r.LANHosts, a.Name+"."+cfg.TailnetDomain)
		}
		if rel := d.Store.CurrentRelease(a.Name); rel != nil && !a.Stopped && rel.Container != "" && rel.Status == state.StatusRunning {
			r.Upstream = fmt.Sprintf("127.0.0.1:%d", rel.HostPort)
		}
		if a.Auth != nil {
			r.AuthUser, r.AuthHash = a.Auth.User, a.Auth.Hash
		}
		routes = append(routes, r)
	}
	conf := caddy.Build(caddy.Options{
		Admin:       cfg.CaddyAdmin,
		HTTPListen:  cfg.CaddyHTTP,
		HTTPSListen: cfg.CaddyHTTPS,
		LANHTTP:     cfg.CaddyLANHTTP,
		StorageDir:  d.Store.Path("caddy"),
		Routes:      routes,
	})
	b, _ := json.MarshalIndent(conf, "", "  ")
	if err := state.WriteFile(d.Store.Path("caddy.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	if d.mdns != nil {
		d.mdns.sync(cfg, apps)
	}
	go tsReg.sync(cfg, apps)
	return d.Caddy.Load(ctx, conf)
}

// customDomains lists every extra hostname across apps.
func (d *Daemon) customDomains() []string {
	apps, _ := d.Store.Apps()
	var out []string
	for _, a := range apps {
		if !a.Private {
			out = append(out, a.Domains...)
		}
	}
	return out
}

// ApplyTunnel writes cloudflared's ingress. With a Cloudflare API token the
// tunnel is remotely managed and updates apply live; without one the local
// config file is rewritten and cloudflared restarted (a brief blip).
func (d *Daemon) ApplyTunnel(ctx context.Context, restartOK bool) (restarted bool, err error) {
	cfg := d.cfg()
	if cfg.NoTunnel || cfg.TunnelID == "" {
		return false, nil
	}
	d.cfMu.Lock()
	defer d.cfMu.Unlock()
	rules := tunnel.Ingress(cfg.Domain, cfg.WebhookAddr, cfg.CaddyHTTP, d.customDomains())
	cred := filepath.Join(d.Store.SecretsDir(), "cloudflared", cfg.TunnelID+".json")
	local := tunnel.LocalConfig(cfg.TunnelID, cred, rules)
	path := d.Store.Path("cloudflared.yml")
	old, _ := os.ReadFile(path)
	if err := state.WriteFile(path, []byte(local), 0o600); err != nil {
		return false, err
	}
	if tok := d.cfToken(); tok != "" && cfg.CFAccountID != "" {
		return false, d.CloudflareAPI(tok).PutTunnelConfig(ctx, cfg.CFAccountID, cfg.TunnelID, rules)
	}
	if string(old) != local && restartOK && cfg.Launchd && launchd.Loaded(launchd.LabelCloudflared) {
		return true, launchd.Restart(launchd.LabelCloudflared)
	}
	return false, nil
}

// ---- lifecycle ----

// Run serves until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	d.Caddy = caddy.NewClient(cfg.CaddyAdmin)
	d.ctx, d.cancel = context.WithCancel(ctx)
	defer d.cancel()
	d.started = time.Now()
	if exe, err := os.Executable(); err == nil {
		d.exe = exe
		if fi, err := os.Stat(exe); err == nil {
			d.exeMod = fi.ModTime()
		}
	}

	sock := d.Store.SocketPath()
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	os.Chmod(sock, 0o600)
	ctlSrv := &http.Server{Handler: d.rpcHandler()}
	go ctlSrv.Serve(ln)

	hookLn, err := net.Listen("tcp", cfg.WebhookAddr)
	if err != nil {
		ln.Close()
		return fmt.Errorf("webhook listener %s: %w", cfg.WebhookAddr, err)
	}
	hookSrv := &http.Server{Handler: d.webhookHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go hookSrv.Serve(hookLn)
	slog.Info("berthd started", "version", Version, "home", d.Store.Root, "socket", sock, "webhooks", cfg.WebhookAddr)

	go func() {
		d.reconcile(d.ctx)
		go d.superviseLoop(d.ctx)
		go d.pollLoop(d.ctx)
		go d.scheduleLoop(d.ctx)
	}()

	<-d.ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hookSrv.Shutdown(sctx)
	ctlSrv.Shutdown(sctx)
	d.stopTails()
	d.mdns.stopAll()
	os.Remove(sock)
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

// reconcile brings the world in line with state: container runtime up,
// network present, services and current releases running, Caddy and the
// tunnel configured. It runs on every start.
func (d *Daemon) reconcile(ctx context.Context) {
	for attempt := 0; !d.RT.SystemRunning(ctx); attempt++ {
		slog.Warn("container system not running; starting it", "attempt", attempt+1)
		if err := d.RT.SystemStart(ctx); err != nil {
			slog.Error("container system start", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(time.Duration(attempt+1)*5*time.Second, time.Minute)):
		}
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return
	}
	gw, err := d.RT.EnsureNetwork(ctx, cfg.Network)
	if err != nil {
		slog.Error("network", "err", err)
	} else if gw != cfg.Gateway {
		slog.Warn("berth network gateway changed; re-linking services", "old", cfg.Gateway, "new", gw)
		old := cfg.Gateway
		cfg.Gateway = gw
		d.Store.SaveConfig(cfg)
		if old != "" {
			defer d.relinkGateway(ctx, old, gw)
		}
	}
	cs, _ := d.RT.List(ctx)
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		if started, err := d.Services.EnsureRunning(ctx, sv, cs); err != nil {
			slog.Error("service start", "service", sv.Kind+":"+sv.Name, "err", err)
		} else if started {
			d.event(state.Event{Kind: "service.start", Message: fmt.Sprintf("%s:%s started", sv.Kind, sv.Name)})
		}
	}
	d.removeOrphans(ctx, cs)
	if err := d.Apply(ctx); err != nil {
		slog.Warn("caddy apply", "err", err)
	}
	apps, _ := d.Store.Apps()
	for _, a := range apps { // oldest first
		rel := d.Store.CurrentRelease(a.Name)
		if rel == nil || a.Stopped {
			continue
		}
		if c, ok := runtime.Find(cs, rel.Container); ok && c.Running() {
			continue
		}
		slog.Info("starting app", "app", a.Name, "release", rel.N)
		if err := d.Deployer.Recover(ctx, a.Name); err != nil {
			d.event(state.Event{Kind: "crash", App: a.Name, Message: fmt.Sprintf("v%d failed to start on boot: %v", rel.N, err)})
		} else {
			d.event(state.Event{Kind: "start", App: a.Name, Message: fmt.Sprintf("v%d started on boot", rel.N)})
		}
	}
	if err := d.Apply(ctx); err != nil {
		slog.Warn("caddy apply", "err", err)
	}
	if _, err := d.ApplyTunnel(ctx, true); err != nil {
		slog.Warn("tunnel apply", "err", err)
	}
}

// removeOrphans deletes berth app containers no release points at (left
// behind by a crash mid-deploy).
func (d *Daemon) removeOrphans(ctx context.Context, cs []runtime.Container) {
	live := map[string]bool{}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		rs, _ := d.Store.Releases(a.Name)
		for _, r := range rs {
			if r.Container != "" {
				live[r.Container] = true
			}
		}
	}
	for _, c := range cs {
		if c.Labels["dev.berth.app"] != "" && !live[c.ID] && !d.Deployer.Busy(c.Labels["dev.berth.app"]) {
			slog.Info("removing orphan container", "container", c.ID)
			d.RT.Remove(ctx, c.ID)
		}
	}
}

// relinkGateway rewrites link URLs after the network gateway changed and
// redeploys affected apps with their current image.
func (d *Daemon) relinkGateway(ctx context.Context, oldGW, newGW string) {
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		env, err := d.Store.Env(a.Name)
		if err != nil {
			continue
		}
		changed := false
		for k, v := range env {
			if strings.Contains(v, "@"+oldGW+":") {
				env[k] = strings.ReplaceAll(v, "@"+oldGW+":", "@"+newGW+":")
				changed = true
			}
		}
		if changed {
			d.Store.SaveEnv(a.Name, env)
			d.Deployer.Deploy(ctx, deploy.Request{App: a.Name, ReuseImage: true, Trigger: "relink"})
		}
	}
}

// selfUpdated reports whether berthd's binary changed on disk (brew upgrade).
func (d *Daemon) selfUpdated() bool {
	if d.exe == "" {
		return false
	}
	fi, err := os.Stat(d.exe)
	return err == nil && !fi.ModTime().Equal(d.exeMod)
}

// reexec replaces the process with the new binary once no deploy runs.
func (d *Daemon) reexec() {
	slog.Info("binary changed; restarting", "exe", d.exe)
	d.event(state.Event{Kind: "berthd.restart", Message: "berthd binary updated; restarting"})
	d.stopTails()
	d.mdns.stopAll()
	os.Remove(d.Store.SocketPath())
	if err := syscall.Exec(d.exe, os.Args, os.Environ()); err != nil {
		slog.Error("re-exec failed; exiting so launchd restarts us", "err", err)
		os.Exit(0)
	}
}
