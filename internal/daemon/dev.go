package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/gitx"
	"github.com/dps/berth/internal/services"
	"github.com/dps/berth/internal/state"
)

// Dev versions (design.md, Dev versions): <app>-dev is an ordinary app
// with DevOf set, built from snapshots pushed to the parent's mirror at
// refs/berth/dev, with its own data.

const devRef = "refs/berth/dev"

// devOwnersGroup is who may open dev versions when sign-in is set up.
const devOwnersGroup = "owners"

// ensureDev returns parent's dev version, creating it on first use.
func (d *Daemon) ensureDev(ctx context.Context, parent *state.App, out io.Writer) (*state.App, error) {
	if parent.DevOff {
		return nil, fmt.Errorf("dev versions are off for %s (berth dev:on %s)", parent.Name, parent.Name)
	}
	return d.ensureChild(ctx, parent, 0, out)
}

// ensureChild returns parent's dev version (pr 0) or the preview of pull
// request pr, creating it with its own data on first use.
func (d *Daemon) ensureChild(ctx context.Context, parent *state.App, pr int, out io.Writer) (_ *state.App, err error) {
	kind, name, branch := "dev version", state.DevName(parent.Name), devRef
	if pr > 0 {
		kind, name, branch = "preview", state.PreviewName(parent.Name, pr), previewRef(pr)
	}
	if parent.DevOf != "" {
		return nil, fmt.Errorf("%s is itself a dev version or preview", parent.Name)
	}
	if a, err := d.Store.App(name); err == nil {
		if a.DevOf != parent.Name {
			return nil, fmt.Errorf("an app called %s already exists, so %s can't have that %s", name, parent.Name, kind)
		}
		return a, nil
	}
	if err := state.ValidateAppName(name); err != nil {
		return nil, fmt.Errorf("%s's name is too long for a %s (%s)", parent.Name, kind, name)
	}
	cfg := d.cfg()
	dev := &state.App{
		Name: name, Branch: branch, Path: parent.Path, DevOf: parent.Name, Preview: pr,
		NoWebhook: true, Private: parent.Private,
		Webhook:   state.Webhook{Secret: randomHex(32)},
		CreatedAt: time.Now(),
	}
	if d.signInHost(cfg) != "" {
		dev.Access = &state.Access{Level: state.LevelAllow, Allow: []string{"@" + devOwnersGroup}}
	} else {
		dev.Private = true // no sign-in: LAN only
	}
	for _, m := range parent.Mounts {
		dev.Mounts = append(dev.Mounts, state.Mount{Path: m.Path, Source: d.Store.Path("data", "apps", name, filepath.Base(m.Source))})
	}
	penv, err := d.Store.Env(parent.Name)
	if err != nil {
		return nil, err
	}
	env := penv.Clone()
	type devLink struct{ kind, name, envVar string }
	var links []devLink
	for _, link := range parent.Links {
		kind, svName, _ := strings.Cut(link, ":")
		sv, err := d.Store.Service(kind, svName)
		if err != nil {
			continue
		}
		l, ok := sv.Links[parent.Name]
		if !ok {
			continue
		}
		delete(env, l.EnvVar) // never production's credentials
		links = append(links, devLink{kind, svName, l.EnvVar})
	}
	env[state.KeyKeepReleases] = "3"
	devAI(cfg, parent, dev, env) // its own token, local models only
	if err := d.Store.SaveApp(dev); err != nil {
		return nil, err
	}
	// Half-made, it would be reused as it is by the next push: undo it so
	// the next one starts over.
	defer func() {
		if err != nil {
			d.destroyApp(context.Background(), dev, false, io.Discard)
			fmt.Fprintf(out, "       removed the half-made %s; the next push tries again\n", name)
		}
	}()
	fmt.Fprintf(out, "-----> Creating %s, the %s of %s (its own data; config copied from %s)\n", name, kind, parent.Name, parent.Name)
	for _, l := range links {
		switch l.kind {
		case "pg":
			db := services.DevDB(l.name)
			if pr > 0 {
				db = services.PreviewDB(l.name, pr)
			}
			url, err := d.Services.LinkDev(ctx, l.name, name, l.envVar, db)
			if err != nil {
				return nil, fmt.Errorf("%s database in pg:%s: %w", kind, l.name, err)
			}
			env[l.envVar] = url
			dev.Links = append(dev.Links, "pg:"+l.name)
			if pr > 0 {
				fmt.Fprintf(out, "-----> %s → database %s in pg:%s (empty; the release command sets it up)\n", l.envVar, db, l.name)
			} else {
				fmt.Fprintf(out, "-----> %s → database %s in pg:%s (empty; berth dev:db:copy %s copies production's)\n", l.envVar, db, l.name, parent.Name)
			}
		case "redis":
			svName := state.DevName(l.name)
			if pr > 0 {
				svName = state.PreviewName(l.name, pr)
			}
			if _, err := d.Store.Service("redis", svName); err != nil {
				// Apple's container gives each container at least 200 MB.
				if _, err := d.Services.Create(ctx, "redis", svName, "", "256M", out); err != nil {
					return nil, fmt.Errorf("dev Redis: %w", err)
				}
			}
			envVar, url, err := d.Services.Link(ctx, "redis", svName, name, l.envVar, env)
			if err != nil {
				return nil, fmt.Errorf("dev Redis: %w", err)
			}
			env[envVar] = url
			dev.Links = append(dev.Links, "redis:"+svName)
			fmt.Fprintf(out, "-----> %s → redis:%s\n", envVar, svName)
		}
	}
	if err := d.Store.SaveEnv(name, env); err != nil {
		return nil, err
	}
	if err := d.Store.SaveApp(dev); err != nil {
		return nil, err
	}
	if err := gitx.EnsureMirror(ctx, d.Store.Path("apps", parent.Name, "repo.git"), ""); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		fmt.Fprintf(out, "       warning: proxy update failed: %v\n", err)
	}
	if pr > 0 {
		d.event(state.Event{Kind: "preview.create", App: name, Message: fmt.Sprintf("preview of %s for pull request #%d created", parent.Name, pr)})
	} else {
		d.event(state.Event{Kind: "dev.create", App: name, Message: "dev version of " + parent.Name + " created"})
	}
	return dev, nil
}

// dropDevServices removes a dev version's database and Redis.
func (d *Daemon) dropDevServices(ctx context.Context, dev *state.App, out io.Writer) {
	for _, link := range dev.Links {
		kind, name, _ := strings.Cut(link, ":")
		switch {
		case kind == "pg":
			if sv, err := d.Store.Service("pg", name); err == nil && sv.Links[dev.Name].DB != "" {
				if err := d.Services.DropDev(ctx, name, dev.Name); err != nil {
					fmt.Fprintf(out, "       warning: dropping the dev database in pg:%s: %v\n", name, err)
				} else {
					fmt.Fprintf(out, "-----> Dropped the dev database in pg:%s\n", name)
				}
			}
		case kind == "redis" && (strings.HasSuffix(name, "-dev") || state.IsPreviewName(name)):
			if sv, err := d.Store.Service("redis", name); err == nil && len(sv.Links) == 1 {
				if _, ok := sv.Links[dev.Name]; ok {
					d.Services.Destroy(ctx, "redis", name, false)
					fmt.Fprintf(out, "-----> Removed redis:%s\n", name)
				}
			}
		}
	}
}

type DevDeployParams struct {
	App     string `json:"app"` // the parent app
	Ref     string `json:"ref"` // the snapshot commit
	Base    string `json:"base,omitempty"`
	Changes string `json:"changes,omitempty"` // git --shortstat of the snapshot against base; empty = clean
}

type DevDeployResult struct {
	App       string  `json:"app"`
	Dev       string  `json:"dev"`
	Release   int     `json:"release,omitempty"`
	Status    string  `json:"status"`          // live | failed
	Stage     string  `json:"stage,omitempty"` // build | release | start, when failed
	URL       string  `json:"url"`
	SHA       string  `json:"sha,omitempty"`
	Base      string  `json:"base,omitempty"`
	Dirty     bool    `json:"dirty"`
	Changes   string  `json:"changes,omitempty"`
	BuildSecs float64 `json:"build_secs,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// hDevDeploy deploys a pushed snapshot to the dev version. Failures are
// reported in the result (not as an RPC error) so the CLI can map the
// stage to an exit code.
func (d *Daemon) hDevDeploy(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DevDeployParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Ref == "" {
		p.Ref = devRef
	} else if err := state.ValidateRef("ref", p.Ref); err != nil {
		return nil, err
	}
	parent, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	dev, err := d.ensureDev(ctx, parent, out)
	if err != nil {
		return nil, err
	}
	res := &DevDeployResult{App: parent.Name, Dev: dev.Name, Base: p.Base, Dirty: p.Changes != "", Changes: p.Changes}
	if urls := AppURLs(d.cfg(), dev); len(urls) > 0 {
		res.URL = urls[0]
	}
	rel, err := d.Deployer.Deploy(ctx, deploy.Request{App: dev.Name, Ref: p.Ref, Trigger: "dev-push", Out: out})
	if rel != nil {
		res.Release, res.SHA, res.BuildSecs = rel.N, rel.SHA, rel.BuildSecs
	}
	if err != nil {
		res.Status, res.Error, res.Stage = "failed", err.Error(), "build"
		if rel != nil && rel.FailedStage != "" {
			res.Stage = rel.FailedStage
		}
		return res, nil
	}
	res.Status = "live"
	return res, nil
}

type DevParams struct {
	App    string `json:"app"`
	Off    bool   `json:"off,omitempty"`
	As     string `json:"as,omitempty"`
	Revoke bool   `json:"revoke,omitempty"`
}

// hDevReset deletes the dev version and its data; the next push starts
// fresh from the parent's current config.
func (d *Daemon) hDevReset(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DevParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	parent, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	dev, err := d.Store.App(state.DevName(parent.Name))
	if err != nil || dev.DevOf != parent.Name {
		fmt.Fprintf(out, "-----> %s has no dev version yet\n", parent.Name)
		return map[string]string{"app": parent.Name}, nil
	}
	d.destroyApp(ctx, dev, false, out)
	fmt.Fprintf(out, "-----> Reset: the next `berth dev:push` creates %s afresh\n", dev.Name)
	return map[string]string{"app": parent.Name, "reset": dev.Name}, nil
}

func (d *Daemon) hDevSet(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DevParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	parent, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if parent.DevOf != "" {
		return nil, fmt.Errorf("%s is itself a dev version", parent.Name)
	}
	if p.Off {
		if dev, err := d.Store.App(state.DevName(parent.Name)); err == nil && dev.DevOf == parent.Name {
			d.destroyApp(ctx, dev, false, out)
		}
	}
	if err := d.updateApp(parent.Name, func(a *state.App) { a.DevOff = p.Off }); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	if p.Off {
		fmt.Fprintf(out, "-----> %s has no dev version or dev address now (berth dev:on %s)\n", parent.Name, parent.Name)
	} else {
		fmt.Fprintf(out, "-----> %s can have a dev version again (berth dev:push)\n", parent.Name)
	}
	return map[string]any{"app": parent.Name, "dev_off": p.Off}, nil
}

func (d *Daemon) hDevToken(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DevParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	p.As = strings.ToLower(strings.TrimSpace(p.As))
	if p.As != "" {
		if err := state.ValidateAllowEntry(p.As); err != nil || strings.HasPrefix(p.As, "@") {
			return nil, fmt.Errorf("--as takes an email address")
		}
	}
	parent, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if parent.DevOf != "" {
		return nil, fmt.Errorf("give the app, not its dev version (berth dev:token %s)", parent.DevOf)
	}
	tok := ""
	err = d.updateApp(parent.Name, func(a *state.App) {
		if p.Revoke {
			a.DevTokens = nil
			return
		}
		tok = auth.NewToken()
		a.DevTokens = append(a.DevTokens, state.DevToken{Hash: auth.HashToken(tok), As: p.As, Created: time.Now()})
	})
	if err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	if p.Revoke {
		fmt.Fprintf(out, "-----> Revoked every dev token for %s\n", parent.Name)
		return map[string]any{"app": parent.Name, "revoked": true}, nil
	}
	msg := "dev token created"
	if p.As != "" {
		msg += " (as " + p.As + ")"
	}
	d.event(state.Event{Kind: "dev.token", App: parent.Name, Message: msg})
	return map[string]string{"app": parent.Name, "token": tok, "as": p.As}, nil
}

func (d *Daemon) hDevDBCopy(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p DevParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	dev, err := d.Store.App(state.DevName(p.App))
	if err != nil || dev.DevOf != p.App {
		return nil, fmt.Errorf("%s has no dev version yet (berth dev:push first)", p.App)
	}
	copied := 0
	for _, link := range dev.Links {
		kind, name, _ := strings.Cut(link, ":")
		if kind != "pg" {
			continue
		}
		fmt.Fprintf(out, "-----> Copying pg:%s's database into %s's dev database\n", name, p.App)
		if err := d.Services.CopyToDev(ctx, name, dev.Name); err != nil {
			return nil, err
		}
		copied++
	}
	if copied == 0 {
		return nil, fmt.Errorf("%s has no Postgres to copy", p.App)
	}
	// A new release on the same image, not a restart: the copy has
	// production's schema, so the release command (migrations) runs again.
	if d.Store.CurrentRelease(dev.Name) != nil {
		if _, err := d.Deployer.Deploy(ctx, deploy.Request{App: dev.Name, ReuseImage: true, Trigger: "db-copy", Out: out}); err != nil {
			fmt.Fprintf(out, "       warning: redeploy %s: %v\n", dev.Name, err)
		}
	}
	return map[string]any{"app": p.App, "copied": copied}, nil
}

// sleepIdleDev stops dev versions with no deploy and no request for
// cfg.DevIdleHours. The next dev:push wakes them.
func (d *Daemon) sleepIdleDev(ctx context.Context) {
	cfg := d.cfg()
	if cfg.DevIdleHours <= 0 {
		return
	}
	idle := time.Duration(cfg.DevIdleHours) * time.Hour
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		if a.DevOf == "" || a.Stopped || d.Deployer.Busy(a.Name) {
			continue
		}
		rel := d.Store.CurrentRelease(a.Name)
		if rel == nil || rel.Container == "" {
			continue
		}
		last := rel.StartedAt
		if last.IsZero() {
			last = rel.CreatedAt
		}
		if logs := d.Store.AccessLogs(a.Name); len(logs) > 0 {
			if fi, err := os.Stat(logs[len(logs)-1]); err == nil && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
		if time.Since(last) < idle {
			continue
		}
		if err := d.Deployer.Stop(ctx, a.Name); err != nil {
			slog.Warn("dev sleep", "app", a.Name, "err", err)
			continue
		}
		d.event(state.Event{Kind: "dev.sleep", App: a.Name, Message: fmt.Sprintf("asleep after %s idle; the next dev:push wakes it", idle)})
	}
}
