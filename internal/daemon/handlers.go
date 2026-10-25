package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/gitx"
	"github.com/dps/berth/internal/rpc"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/services"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

type handler func(ctx context.Context, params json.RawMessage, out io.Writer) (any, error)

func (d *Daemon) handlers() map[string]handler {
	return map[string]handler{
		"ping": func(context.Context, json.RawMessage, io.Writer) (any, error) {
			return map[string]string{"version": Version}, nil
		},
		"apps.create":     d.hCreate,
		"apps.destroy":    d.hDestroy,
		"deploy":          d.hDeploy,
		"rollback":        d.hRollback,
		"restart":         d.hRestart,
		"stop":            d.hStop,
		"start":           d.hStart,
		"reload":          d.hReload,
		"config.set":      d.hConfigSet,
		"domains.add":     d.hDomainsAdd,
		"domains.remove":  d.hDomainsRemove,
		"auth.set":        d.hAuthSet,
		"auth.unset":      d.hAuthUnset,
		"storage.mount":   d.hStorageMount,
		"storage.unmount": d.hStorageUnmount,
		"svc.create":      d.hSvcCreate,
		"svc.link":        d.hSvcLink,
		"svc.unlink":      d.hSvcUnlink,
		"svc.destroy":     d.hSvcDestroy,
		"svc.backup":      d.hSvcBackup,
		"svc.restore":     d.hSvcRestore,
		"svc.expose":      d.hSvcExpose,
		"svc.upgrade":     d.hSvcUpgrade,
		"status":          d.hStatus,
		"gc":              d.hGC,
		"backup":          d.hBackup,
		"restore":         d.hRestore,
		"notify.set":      d.hNotifySet,
		"app.setkey":      d.hSetKey,
		"git.hook":        d.hGitHook,
		"dockerfile":      d.hDockerfile,
		"tunnel.apply":    d.hTunnelApply,
	}
}

func (d *Daemon) rpcHandler() http.Handler {
	hs := d.handlers()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rpc/{method}", func(w http.ResponseWriter, r *http.Request) {
		h, ok := hs[r.PathValue("method")]
		if !ok {
			http.Error(w, "unknown method "+r.PathValue("method"), http.StatusNotFound)
			return
		}
		params, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/x-ndjson")
		s := rpc.NewStream(w)
		// Operations outlive a disconnected CLI (ctrl-c must not leave a
		// half-swapped deploy), so they run on the daemon's context.
		res, err := h(d.ctx, params, s)
		s.Finish(res, err)
	})
	return mux
}

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	return json.Unmarshal(params, v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AppURLs returns the URLs an app is reachable at.
func AppURLs(cfg *state.Config, a *state.App) []string {
	var urls []string
	if !a.Private && !cfg.NoTunnel && cfg.Domain != "" {
		urls = append(urls, "https://"+a.Name+"."+cfg.Domain)
		for _, dom := range a.Domains {
			urls = append(urls, "https://"+dom)
		}
	}
	if cfg.Hostname != "" && cfg.CaddyHTTPS != "" {
		u := "https://" + a.Name + "." + cfg.Hostname + ".local"
		if p := portSuffix(cfg.CaddyHTTPS, "443"); p != "" {
			u += p
		}
		urls = append(urls, u)
	}
	if cfg.Tailscale && cfg.TailnetDomain != "" {
		urls = append(urls, "https://"+a.Name+"."+cfg.TailnetDomain)
	}
	return urls
}

func portSuffix(addr, def string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 || addr[i+1:] == def {
		return ""
	}
	return ":" + addr[i+1:]
}

// ---- apps ----

type CreateParams struct {
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Path      string `json:"path"`
	Private   bool   `json:"private"`
	NoWebhook bool   `json:"no_webhook"`
	NoDeploy  bool   `json:"no_deploy"`
}

type CreateResult struct {
	App     string   `json:"app"`
	URLs    []string `json:"urls"`
	Webhook string   `json:"webhook"` // registered | polling | none
	Release int      `json:"release,omitempty"`
}

func (d *Daemon) hCreate(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p CreateParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := state.ValidateAppName(p.Name); err != nil {
		return nil, err
	}
	if d.Store.AppExists(p.Name) {
		return nil, fmt.Errorf("app %q already exists", p.Name)
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return nil, err
	}
	if p.Branch == "" {
		p.Branch = "main"
	}
	app := &state.App{
		Name: p.Name, Repo: p.Repo, Branch: p.Branch, Path: strings.Trim(p.Path, "/"),
		Private: p.Private, NoWebhook: p.NoWebhook,
		Webhook:   state.Webhook{Secret: randomHex(32)},
		CreatedAt: time.Now(),
	}
	if err := d.Store.SaveApp(app); err != nil {
		return nil, err
	}
	if err := d.Store.SaveEnv(app.Name, state.Env{}); err != nil {
		return nil, err
	}
	res := &CreateResult{App: app.Name, URLs: AppURLs(cfg, app), Webhook: "none"}
	if err := gitx.EnsureMirror(ctx, d.Store.Path("apps", app.Name, "repo.git"), cloneURLOrEmpty(app)); err != nil {
		return nil, err
	}
	d.installHook(app)

	switch {
	case app.Repo == "":
		fmt.Fprintf(out, "-----> No --repo: deploy with `git push berth %s` (see `berth git:remote %s`) or `berth deploy`\n", app.Branch, app.Name)
	case app.IsGitHub() && !p.NoWebhook && !cfg.NoTunnel && cfg.Domain != "" && d.githubToken() != "":
		hookURL := fmt.Sprintf("https://hooks.%s/github/%s", cfg.Domain, app.Name)
		id, err := d.GitHubAPI(d.githubToken()).CreateHook(ctx, app.Repo, hookURL, app.Webhook.Secret)
		if err != nil {
			fmt.Fprintf(out, "-----> Could not register a webhook on github.com/%s (%v); polling every 60 s instead\n", app.Repo, err)
			res.Webhook = "polling"
		} else {
			d.updateApp(app.Name, func(a *state.App) { a.Webhook.ID = id })
			fmt.Fprintf(out, "-----> Registered webhook on github.com/%s\n", app.Repo)
			res.Webhook = "registered"
		}
	default:
		fmt.Fprintf(out, "-----> Watching %s@%s by polling every 60 s\n", app.Repo, app.Branch)
		res.Webhook = "polling"
	}
	if err := d.Apply(ctx); err != nil {
		fmt.Fprintf(out, "       warning: proxy update failed: %v\n", err)
	}
	for _, u := range res.URLs {
		fmt.Fprintf(out, "-----> %s\n", u)
	}
	fmt.Fprintf(out, "       Your app must: listen on $PORT on 0.0.0.0; read config from env;\n")
	fmt.Fprintf(out, "       write only to /tmp or a mounted volume; log to stdout.\n")
	if app.Repo == "" || p.NoDeploy {
		return res, nil
	}
	rel, err := d.Deployer.Deploy(ctx, deploy.Request{App: app.Name, Trigger: "create", Out: out})
	if rel != nil {
		res.Release = rel.N
		d.updateApp(app.Name, func(a *state.App) { a.LastSeen = rel.SHA })
	}
	if err != nil {
		return res, fmt.Errorf("app created, but the first deploy failed: %w (fix and `git push`, or `berth deploy %s`)", err, app.Name)
	}
	return res, nil
}

func cloneURLOrEmpty(a *state.App) string {
	if a.Repo == "" {
		return ""
	}
	return a.CloneURL()
}

// installHook writes the post-receive hook for `git push berth`.
func (d *Daemon) installHook(a *state.App) {
	bin := berthBinary()
	gitx.InstallPostReceive(d.Store.Path("apps", a.Name, "repo.git"), bin, d.Store.Root, a.Name, a.Branch)
}

// berthBinary finds the CLI next to berthd.
func berthBinary() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "berth")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("berth"); err == nil {
		return p
	}
	return "berth"
}

type AppParams struct {
	App      string `json:"app"`
	Ref      string `json:"ref,omitempty"`
	N        int    `json:"n,omitempty"`
	NoCache  bool   `json:"no_cache,omitempty"`
	KeepData bool   `json:"keep_data,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
}

func (d *Daemon) hDestroy(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	unlock := d.Deployer.Lock(app.Name)
	defer unlock()
	cfg := d.cfg()
	rs, _ := d.Store.Releases(app.Name)
	var images []string
	for _, r := range rs {
		if r.Container != "" {
			d.RT.Remove(ctx, r.Container)
		}
		if !r.Pruned && r.Image != "" && !slices.Contains(images, r.Image) {
			images = append(images, r.Image)
		}
	}
	// Stray containers labelled for this app.
	if cs, err := d.RT.List(ctx); err == nil {
		for _, c := range cs {
			if c.Labels["dev.berth.app"] == app.Name {
				d.RT.Remove(ctx, c.ID)
			}
		}
	}
	if imgs, err := d.RT.Images(ctx); err == nil {
		var existing []string
		for _, i := range imgs {
			if slices.Contains(images, i.Name) {
				existing = append(existing, i.Name)
			}
		}
		if len(existing) > 0 {
			d.RT.RemoveImage(ctx, existing...)
		}
	}
	fmt.Fprintf(out, "-----> Removed containers and %d image(s)\n", len(images))
	if app.Webhook.ID != 0 && app.IsGitHub() {
		if err := d.GitHubAPI(d.githubToken()).DeleteHook(ctx, app.Repo, app.Webhook.ID); err != nil {
			fmt.Fprintf(out, "       warning: could not delete webhook on github.com/%s: %v\n", app.Repo, err)
		} else {
			fmt.Fprintf(out, "-----> Deleted webhook on github.com/%s\n", app.Repo)
		}
	}
	for _, link := range app.Links {
		kind, name, _ := strings.Cut(link, ":")
		if _, err := d.Services.Unlink(ctx, kind, name, app.Name); err == nil {
			fmt.Fprintf(out, "-----> Unlinked %s\n", link)
		}
	}
	if len(app.Domains) > 0 {
		d.removeDomainDNS(ctx, cfg, app.Domains, out)
	}
	os.RemoveAll(d.Store.AppDir(app.Name))
	if !p.KeepData {
		os.RemoveAll(d.Store.Path("data", "apps", app.Name))
	} else if len(app.Mounts) > 0 {
		fmt.Fprintf(out, "-----> Kept data in %s\n", d.Store.Path("data", "apps", app.Name))
	}
	d.mu.Lock()
	delete(d.crash, app.Name)
	d.mu.Unlock()
	d.Apply(ctx)
	if len(app.Domains) > 0 {
		d.ApplyTunnel(ctx, true)
	}
	d.event(state.Event{Kind: "destroy", App: app.Name, Message: "app destroyed"})
	return map[string]string{"destroyed": app.Name}, nil
}

func (d *Daemon) hDeploy(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	trigger := p.Trigger
	if trigger == "" {
		trigger = "cli"
	}
	rel, err := d.Deployer.Deploy(ctx, deploy.Request{App: p.App, Ref: p.Ref, NoCache: p.NoCache, Trigger: trigger, Out: out})
	if rel != nil && rel.SHA != "" && p.Ref == "" {
		d.updateApp(p.App, func(a *state.App) { a.LastSeen = rel.SHA })
	}
	if err != nil {
		return nil, err
	}
	return d.releaseResult(p.App, rel), nil
}

type ReleaseResult struct {
	App     string   `json:"app"`
	Release int      `json:"release"`
	SHA     string   `json:"sha"`
	URLs    []string `json:"urls"`
}

func (d *Daemon) releaseResult(app string, rel *state.Release) *ReleaseResult {
	r := &ReleaseResult{App: app, Release: rel.N, SHA: rel.SHA}
	if a, err := d.Store.App(app); err == nil {
		r.URLs = AppURLs(d.cfg(), a)
	}
	return r
}

func (d *Daemon) hRollback(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	rel, err := d.Deployer.Rollback(ctx, p.App, p.N, out)
	if err != nil {
		return nil, err
	}
	return d.releaseResult(p.App, rel), nil
}

func (d *Daemon) hRestart(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	delete(d.crash, p.App)
	d.mu.Unlock()
	if err := d.Deployer.Restart(ctx, p.App, out); err != nil {
		if errors.Is(err, deploy.ErrNothingToDeploy) {
			return nil, fmt.Errorf("%s has no release yet; run `berth deploy %s`", p.App, p.App)
		}
		// A crashed app has no container to swap from; start it fresh. Never
		// do this while the current container is running: Recover replaces it.
		rel := d.Store.CurrentRelease(p.App)
		cs, lerr := d.RT.List(ctx)
		if rel == nil || lerr != nil {
			return nil, err
		}
		if c, ok := runtime.Find(cs, rel.Container); ok && c.Running() {
			return nil, err
		}
		if err2 := d.Deployer.Recover(ctx, p.App); err2 != nil {
			return nil, err
		}
	}
	return d.releaseResult(p.App, d.Store.CurrentRelease(p.App)), nil
}

func (d *Daemon) hStop(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := d.Deployer.Stop(ctx, p.App); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Stopped %s (requests get 503 until `berth start %s`)\n", p.App, p.App)
	return map[string]string{"stopped": p.App}, nil
}

func (d *Daemon) hStart(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	delete(d.crash, p.App)
	d.mu.Unlock()
	if err := d.Deployer.Start(ctx, p.App, out); err != nil {
		return nil, err
	}
	return d.releaseResult(p.App, d.Store.CurrentRelease(p.App)), nil
}

// hReload re-runs reconciliation (after hand edits to ~/.berth).
func (d *Daemon) hReload(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	fmt.Fprintln(out, "-----> Reconciling containers, Caddy and the tunnel with ~/.berth")
	d.reconcile(ctx)
	return map[string]bool{"ok": true}, nil
}

// ---- config ----

type ConfigParams struct {
	App       string            `json:"app"`
	Set       map[string]string `json:"set"`
	Unset     []string          `json:"unset"`
	NoRestart bool              `json:"no_restart"`
	BuildArgs []string          `json:"build_args"` // keys to mark as build args
	Replace   bool              `json:"replace"`    // config:import --replace
}

func (d *Daemon) hConfigSet(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ConfigParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := d.Store.App(p.App); err != nil {
		return nil, err
	}
	unlock := d.Deployer.Lock(p.App)
	env, err := d.Store.Env(p.App)
	if err != nil {
		unlock()
		return nil, err
	}
	if p.Replace {
		kept := state.Env{}
		for k, v := range env {
			if strings.HasPrefix(k, "BERTH_") {
				kept[k] = v
			}
		}
		env = kept
	}
	for k, v := range p.Set {
		if err := state.ValidateEnvKey(k); err != nil {
			unlock()
			return nil, err
		}
		env[k] = v
	}
	for _, k := range p.Unset {
		delete(env, k)
	}
	if len(p.BuildArgs) > 0 {
		cur := strings.Split(env["BERTH_BUILD_ARGS"], ",")
		for _, k := range p.BuildArgs {
			if !slices.Contains(cur, k) {
				cur = append(cur, k)
			}
		}
		env["BERTH_BUILD_ARGS"] = strings.Trim(strings.Join(cur, ","), ",")
	}
	err = d.Store.SaveEnv(p.App, env)
	unlock()
	if err != nil {
		return nil, err
	}
	var keys []string
	for k := range p.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		fmt.Fprintf(out, "-----> Set %s\n", strings.Join(keys, ", "))
	}
	if len(p.Unset) > 0 {
		fmt.Fprintf(out, "-----> Unset %s\n", strings.Join(p.Unset, ", "))
	}
	return d.redeployForConfig(ctx, p.App, p.NoRestart, len(p.BuildArgs) > 0, out)
}

// redeployForConfig starts a new release with the current image and new
// env (or rebuilds when build args changed).
func (d *Daemon) redeployForConfig(ctx context.Context, app string, noRestart, rebuild bool, out io.Writer) (any, error) {
	if noRestart {
		fmt.Fprintf(out, "       Not restarting; takes effect on the next deploy or `berth deploy %s`\n", app)
		return map[string]bool{"restarted": false}, nil
	}
	if d.Store.CurrentRelease(app) == nil {
		fmt.Fprintln(out, "       No release yet; takes effect on the first deploy")
		return map[string]bool{"restarted": false}, nil
	}
	rel, err := d.Deployer.Deploy(ctx, deploy.Request{App: app, ReuseImage: !rebuild, Trigger: "config", Out: out})
	if err != nil {
		return nil, err
	}
	return d.releaseResult(app, rel), nil
}

// ---- domains ----

type DomainParams struct {
	App   string `json:"app"`
	Host  string `json:"host"`
	Force bool   `json:"force,omitempty"` // replace an existing DNS record
}

func validHost(h string) error {
	if h == "" || strings.ContainsAny(h, "/: ") || !strings.Contains(h, ".") {
		return fmt.Errorf("invalid hostname %q", h)
	}
	return nil
}

func (d *Daemon) hDomainsAdd(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DomainParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	p.Host = strings.ToLower(strings.TrimSuffix(p.Host, "."))
	if err := validHost(p.Host); err != nil {
		return nil, err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return nil, err
	}
	if cfg.NoTunnel {
		return nil, fmt.Errorf("custom domains need the Cloudflare tunnel (berth was set up with --no-tunnel)")
	}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		if slices.Contains(a.Domains, p.Host) {
			return nil, fmt.Errorf("%s is already attached to %s", p.Host, a.Name)
		}
	}
	// DNS first: if the hostname already points somewhere, stop before
	// changing anything.
	if err := d.addDomainDNS(ctx, cfg, p.Host, p.Force, out); err != nil {
		return nil, err
	}
	if err := d.updateApp(p.App, func(a *state.App) { a.Domains = append(a.Domains, p.Host) }); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	restarted, err := d.ApplyTunnel(ctx, true)
	if err != nil {
		return nil, err
	}
	if restarted {
		fmt.Fprintln(out, "       warning: cloudflared was restarted to pick up the new hostname (in-flight requests may have dropped)")
	}
	fmt.Fprintf(out, "-----> https://%s\n", p.Host)
	return map[string]string{"app": p.App, "url": "https://" + p.Host}, nil
}

// addDomainDNS points host at the tunnel. Zones connected with a browser
// login (`berth init` for the main domain, `berth cf:login <zone>` for
// others) use `cloudflared tunnel route dns` with that zone's cert; a scoped
// API token, if configured, covers any zone it can see; otherwise the
// record to create is printed.
func (d *Daemon) addDomainDNS(ctx context.Context, cfg *state.Config, host string, force bool, out io.Writer) error {
	target := cfg.TunnelID + ".cfargotunnel.com"
	// existing refuses to replace a record that points somewhere else.
	existing := func(api *tunnel.API, zoneID string) error {
		recs, err := api.Records(ctx, zoneID, host)
		if err != nil || force {
			return nil
		}
		for _, r := range recs {
			if !(r.Type == "CNAME" && r.Content == target) {
				return fmt.Errorf("%s already has a DNS record (%s %s); pass --force to replace it with the tunnel", host, r.Type, r.Content)
			}
		}
		return nil
	}
	if zone, cert := tunnel.CertForHost(d.zoneCertDir(), host); cert != "" {
		if oc, err := tunnel.ReadOriginCert(cert); err == nil {
			if err := existing(d.CloudflareAPI(oc.APIToken), oc.ZoneID); err != nil {
				return err
			}
		}
		cli := &tunnel.CLI{Bin: runtime.LookBin("cloudflared"), Origin: cert}
		if err := cli.RouteDNS(ctx, cfg.TunnelID, host); err != nil {
			return err
		}
		fmt.Fprintf(out, "-----> DNS: %s CNAME %s (proxied, zone %s)\n", host, target, zone)
		return nil
	}
	if tok := d.cfToken(); tok != "" {
		api := d.CloudflareAPI(tok)
		if zone, err := api.ZoneFor(ctx, host); err == nil {
			if err := existing(api, zone.ID); err != nil {
				return err
			}
			if err := api.UpsertCNAME(ctx, zone.ID, host, target); err != nil {
				return err
			}
			fmt.Fprintf(out, "-----> DNS: %s CNAME %s (proxied)\n", host, target)
			return nil
		}
	}
	fmt.Fprintf(out, "-----> %s is not in a zone Berth is logged in to. Either connect the zone\n", host)
	fmt.Fprintf(out, "       (it opens the browser) and run domains:add again:  berth cf:login <zone>\n")
	fmt.Fprintf(out, "       or create this record at your DNS provider:  %s CNAME %s (proxied)\n", host, target)
	return nil
}

func (d *Daemon) removeDomainDNS(ctx context.Context, cfg *state.Config, hosts []string, out io.Writer) {
	target := cfg.TunnelID + ".cfargotunnel.com"
	for _, h := range hosts {
		var err error
		if _, cert := tunnel.CertForHost(d.zoneCertDir(), h); cert != "" {
			var oc *tunnel.OriginCert
			if oc, err = tunnel.ReadOriginCert(cert); err == nil {
				err = d.CloudflareAPI(oc.APIToken).DeleteCNAME(ctx, oc.ZoneID, h, target)
			}
		} else if tok := d.cfToken(); tok != "" {
			api := d.CloudflareAPI(tok)
			var zone *tunnel.Zone
			if zone, err = api.ZoneFor(ctx, h); err == nil {
				err = api.DeleteCNAME(ctx, zone.ID, h, target)
			}
		} else {
			continue
		}
		if err == nil {
			fmt.Fprintf(out, "-----> Removed DNS record for %s\n", h)
		} else {
			fmt.Fprintf(out, "       could not remove the DNS record for %s (%v); delete it in the Cloudflare dashboard\n", h, err)
		}
	}
}

func (d *Daemon) hDomainsRemove(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DomainParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	p.Host = strings.ToLower(p.Host)
	found := false
	if err := d.updateApp(p.App, func(a *state.App) {
		if i := slices.Index(a.Domains, p.Host); i >= 0 {
			a.Domains = slices.Delete(a.Domains, i, i+1)
			found = true
		}
	}); err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s is not attached to %s", p.Host, p.App)
	}
	d.Apply(ctx)
	d.removeDomainDNS(ctx, d.cfg(), []string{p.Host}, out)
	if _, err := d.ApplyTunnel(ctx, true); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Removed %s from %s\n", p.Host, p.App)
	return map[string]string{"removed": p.Host}, nil
}

// ---- auth ----

type AuthParams struct {
	App      string `json:"app"`
	User     string `json:"user"`
	Password string `json:"password"`
}

func (d *Daemon) hAuthSet(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AuthParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.User == "" || p.Password == "" {
		return nil, fmt.Errorf("usage: berth auth:set <app> user:password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	if err := d.updateApp(p.App, func(a *state.App) { a.Auth = &state.BasicAuth{User: p.User, Hash: string(hash)} }); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Basic auth on for %s (user %s)\n", p.App, p.User)
	return map[string]string{"app": p.App, "user": p.User}, nil
}

func (d *Daemon) hAuthUnset(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AuthParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := d.updateApp(p.App, func(a *state.App) { a.Auth = nil }); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Basic auth off for %s\n", p.App)
	return map[string]string{"app": p.App}, nil
}

// ---- storage ----

type StorageParams struct {
	App       string `json:"app"`
	Path      string `json:"path"`
	NoRestart bool   `json:"no_restart"`
}

func (d *Daemon) hStorageMount(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p StorageParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.Path, "/") || p.Path == "/" || strings.Contains(p.Path, "..") {
		return nil, fmt.Errorf("mount path must be an absolute path inside the container, like /data")
	}
	src := d.Store.Path("data", "apps", p.App, strings.ReplaceAll(strings.Trim(p.Path, "/"), "/", "_"))
	err := d.updateApp(p.App, func(a *state.App) {
		for _, m := range a.Mounts {
			if m.Path == p.Path {
				return
			}
		}
		a.Mounts = append(a.Mounts, state.Mount{Path: p.Path, Source: src})
	})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> %s → %s (persists across deploys)\n", src, p.Path)
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

func (d *Daemon) hStorageUnmount(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p StorageParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	found := false
	err := d.updateApp(p.App, func(a *state.App) {
		for i, m := range a.Mounts {
			if m.Path == p.Path {
				a.Mounts = slices.Delete(a.Mounts, i, i+1)
				found = true
				return
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s has no mount at %s", p.App, p.Path)
	}
	fmt.Fprintf(out, "-----> Unmounted %s (data kept in %s)\n", p.Path, d.Store.Path("data", "apps", p.App))
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

// ---- services ----

type SvcParams struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	App       string `json:"app,omitempty"`
	Image     string `json:"image,omitempty"`
	Memory    string `json:"memory,omitempty"`
	As        string `json:"as,omitempty"`
	Force     bool   `json:"force,omitempty"`
	KeepData  bool   `json:"keep_data,omitempty"`
	File      string `json:"file,omitempty"`
	Port      int    `json:"port,omitempty"`
	Version   string `json:"version,omitempty"`
	NoRestart bool   `json:"no_restart,omitempty"`
}

func (d *Daemon) hSvcCreate(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	sv, err := d.Services.Create(ctx, p.Kind, p.Name, p.Image, p.Memory, out)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> %s:%s is up; link it with `berth %s:link %s <app>`\n", p.Kind, p.Name, p.Kind, p.Name)
	d.event(state.Event{Kind: "service.create", Message: fmt.Sprintf("%s:%s created (%s)", sv.Kind, sv.Name, sv.Image)})
	return sv.Name, nil
}

func (d *Daemon) hSvcLink(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := d.Store.App(p.App); err != nil {
		return nil, err
	}
	env, err := d.Store.Env(p.App)
	if err != nil {
		return nil, err
	}
	envVar, url, err := d.Services.Link(ctx, p.Kind, p.Name, p.App, p.As, env)
	if err != nil {
		return nil, err
	}
	link := p.Kind + ":" + p.Name
	d.updateApp(p.App, func(a *state.App) {
		if !slices.Contains(a.Links, link) {
			a.Links = append(a.Links, link)
		}
	})
	unlock := d.Deployer.Lock(p.App)
	env, _ = d.Store.Env(p.App)
	env[envVar] = url
	err = d.Store.SaveEnv(p.App, env)
	unlock()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Linked %s to %s as %s\n", link, p.App, envVar)
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

func (d *Daemon) hSvcUnlink(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	envVar, err := d.Services.Unlink(ctx, p.Kind, p.Name, p.App)
	if err != nil {
		return nil, err
	}
	link := p.Kind + ":" + p.Name
	d.updateApp(p.App, func(a *state.App) {
		if i := slices.Index(a.Links, link); i >= 0 {
			a.Links = slices.Delete(a.Links, i, i+1)
		}
	})
	unlock := d.Deployer.Lock(p.App)
	env, _ := d.Store.Env(p.App)
	delete(env, envVar)
	d.Store.SaveEnv(p.App, env)
	unlock()
	fmt.Fprintf(out, "-----> Unlinked %s from %s (removed %s, revoked its login)\n", link, p.App, envVar)
	return d.redeployForConfig(ctx, p.App, p.NoRestart, false, out)
}

func (d *Daemon) hSvcDestroy(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	sv, err := d.Store.Service(p.Kind, p.Name)
	if err != nil {
		return nil, err
	}
	if len(sv.Links) > 0 && !p.Force {
		var apps []string
		for a := range sv.Links {
			apps = append(apps, a)
		}
		sort.Strings(apps)
		return nil, fmt.Errorf("%s:%s is linked to %s; unlink first or pass --force", p.Kind, p.Name, strings.Join(apps, ", "))
	}
	for a := range sv.Links {
		link := p.Kind + ":" + p.Name
		d.updateApp(a, func(x *state.App) {
			if i := slices.Index(x.Links, link); i >= 0 {
				x.Links = slices.Delete(x.Links, i, i+1)
			}
		})
	}
	if err := d.Services.Destroy(ctx, p.Kind, p.Name, p.KeepData); err != nil {
		return nil, err
	}
	msg := "volume deleted"
	if p.KeepData {
		msg = "volume " + sv.Volume + " kept"
	}
	fmt.Fprintf(out, "-----> Destroyed %s:%s (%s; backups in %s)\n", p.Kind, p.Name, msg, d.Store.Path("backups", p.Kind, p.Name))
	d.event(state.Event{Kind: "service.destroy", Message: fmt.Sprintf("%s:%s destroyed", p.Kind, p.Name)})
	return map[string]string{"destroyed": p.Kind + ":" + p.Name}, nil
}

func (d *Daemon) hSvcBackup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	path, err := d.Services.Backup(ctx, p.Kind, p.Name, 0)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(path)
	fmt.Fprintf(out, "-----> %s (%s)\n", path, deploy.HumanBytes(fi.Size()))
	return map[string]string{"file": path}, nil
}

func (d *Daemon) hSvcRestore(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.File); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Restoring %s into %s:%s\n", filepath.Base(p.File), p.Kind, p.Name)
	if err := d.Services.Restore(ctx, p.Kind, p.Name, p.File); err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "-----> Restored")
	return map[string]string{"restored": p.File}, nil
}

func (d *Daemon) hSvcExpose(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := d.Services.Expose(ctx, p.Kind, p.Name, p.Port); err != nil {
		return nil, err
	}
	if p.Port == 0 {
		fmt.Fprintf(out, "-----> %s:%s no longer published on localhost\n", p.Kind, p.Name)
	} else {
		sv, _ := d.Store.Service(p.Kind, p.Name)
		fmt.Fprintf(out, "-----> %s:%s on 127.0.0.1:%d\n", p.Kind, p.Name, p.Port)
		if p.Kind == "pg" {
			fmt.Fprintf(out, "       postgres://postgres:%s@127.0.0.1:%d/%s\n", sv.Password, p.Port, strings.ReplaceAll(p.Name, "-", "_"))
		}
	}
	return map[string]int{"port": p.Port}, nil
}

func (d *Daemon) hSvcUpgrade(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p SvcParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Kind != "pg" {
		return nil, fmt.Errorf("upgrade is only supported for pg")
	}
	oldVolume, err := d.Services.Upgrade(ctx, p.Name, p.Version, out)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> pg:%s now runs postgres:%s; old data kept in volume %s\n", p.Name, p.Version, oldVolume)
	fmt.Fprintf(out, "       delete it once you're happy: container volume delete %s\n", oldVolume)
	d.event(state.Event{Kind: "service.upgrade", Message: fmt.Sprintf("pg:%s upgraded to %s", p.Name, p.Version)})
	return map[string]string{"old_volume": oldVolume}, nil
}

// ---- misc ----

func (d *Daemon) hGC(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		d.Deployer.GC(ctx, a.Name, out)
	}
	cfg := d.cfg()
	removed := 0
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		removed += services.Prune(d.Store.Path("backups", sv.Kind, sv.Name), cfg.PGBackupKeep)
	}
	if removed > 0 {
		fmt.Fprintf(out, "-----> Removed %d old backup(s)\n", removed)
	}
	// Leftover build directories from interrupted deploys.
	dirs, _ := filepath.Glob(d.Store.Path("apps", "*", "build", "*"))
	for _, dir := range dirs {
		app := filepath.Base(filepath.Dir(filepath.Dir(dir)))
		if !d.Deployer.Busy(app) {
			os.RemoveAll(dir)
		}
	}
	free, _ := deploy.FreeBytes(d.Store.Root)
	fmt.Fprintf(out, "-----> Done; %s free\n", deploy.HumanBytes(int64(free)))
	return map[string]uint64{"free_bytes": free}, nil
}

func (d *Daemon) hNotifySet(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p struct {
		URL  string `json:"url"`
		Test bool   `json:"test"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return nil, err
	}
	cfg.NotifyURL = p.URL
	if err := d.Store.SaveConfig(cfg); err != nil {
		return nil, err
	}
	if p.URL == "" {
		fmt.Fprintln(out, "-----> Notifications off")
		return map[string]string{"url": ""}, nil
	}
	if err := SendNotification(p.URL, cfg.Domain, state.Event{Kind: "test", Message: "berth notifications are working", Time: time.Now()}); err != nil {
		return nil, fmt.Errorf("saved, but the test notification failed: %w", err)
	}
	fmt.Fprintln(out, "-----> Sent a test notification")
	return map[string]string{"url": p.URL}, nil
}

// hSetKey switches an app to an SSH deploy key.
func (d *Daemon) hSetKey(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if !app.IsGitHub() {
		return nil, fmt.Errorf("deploy keys are for GitHub repos (owner/name)")
	}
	keyDir := filepath.Join(d.Store.SecretsDir(), "keys")
	os.MkdirAll(keyDir, 0o700)
	key := filepath.Join(keyDir, app.Name)
	if _, err := os.Stat(key); err != nil {
		cmd := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "berth-"+app.Name, "-f", key)
		if b, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ssh-keygen: %v: %s", err, b)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return nil, err
	}
	if err := d.updateApp(app.Name, func(a *state.App) { a.DeployKey = true }); err != nil {
		return nil, err
	}
	gitx.EnsureMirror(ctx, d.Store.Path("apps", app.Name, "repo.git"), "git@github.com:"+app.Repo+".git")
	added := false
	if tok := d.githubToken(); tok != "" {
		if err := d.GitHubAPI(tok).AddDeployKey(ctx, app.Repo, "berth "+app.Name, strings.TrimSpace(string(pub))); err == nil {
			added = true
			fmt.Fprintf(out, "-----> Added read-only deploy key to github.com/%s\n", app.Repo)
		}
	}
	if !added {
		fmt.Fprintf(out, "-----> Add this read-only deploy key at https://github.com/%s/settings/keys:\n\n%s\n", app.Repo, strings.TrimSpace(string(pub)))
	}
	return map[string]any{"public_key": strings.TrimSpace(string(pub)), "added": added}, nil
}

// hGitHook (re)installs the post-receive hook so `git push berth` deploys.
func (d *Daemon) hGitHook(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	repo := d.Store.Path("apps", app.Name, "repo.git")
	if err := gitx.EnsureMirror(ctx, repo, cloneURLOrEmpty(app)); err != nil {
		return nil, err
	}
	d.installHook(app)
	return map[string]string{"repo": repo, "branch": app.Branch}, nil
}

// hDockerfile prints the Dockerfile a deploy would use right now.
func (d *Daemon) hDockerfile(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	app, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	env, _ := d.Store.Env(app.Name)
	repo := d.Store.Path("apps", app.Name, "repo.git")
	if err := gitx.EnsureMirror(ctx, repo, cloneURLOrEmpty(app)); err != nil {
		return nil, err
	}
	if app.Repo != "" {
		gitx.Fetch(ctx, repo, app.Branch, d.gitAuth(app))
	}
	ref := p.Ref
	if ref == "" {
		ref = app.Branch
	}
	sha, err := gitx.Resolve(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "berth-df-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := gitx.Export(ctx, repo, sha, app.Path, dir); err != nil {
		return nil, err
	}
	path, stack, err := deploy.ResolveDockerfile(dir, env)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"stack": stack, "dockerfile": string(b)}, nil
}

func (d *Daemon) hTunnelApply(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	restarted, err := d.ApplyTunnel(ctx, true)
	if err != nil {
		return nil, err
	}
	if d.cfg().CFIPFilter {
		if err := d.refreshIPFilter(ctx); err != nil {
			fmt.Fprintf(out, "       warning: webhook IP filter: %v\n", err)
		}
	}
	return map[string]bool{"restarted": restarted}, nil
}
