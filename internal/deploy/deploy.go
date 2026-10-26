// Package deploy is the release pipeline: fetch → dockerfile → build → run →
// health → swap → gc, plus rollback, restart, stop/start and crash
// recovery, which all reuse the same start-check-swap path.
package deploy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dps/berth/internal/detect"
	"github.com/dps/berth/internal/gitx"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// Router re-applies proxy config after current releases change.
type Router interface {
	Apply(ctx context.Context) error
}

type Deployer struct {
	Store  *state.Store
	RT     runtime.Runtime
	Router Router
	// GitAuth returns fetch credentials for an app.
	GitAuth func(a *state.App) gitx.Auth
	// Event records an event (events.log, notifications).
	Event func(ev state.Event)
	// Backup dumps the app's Postgres databases before a release command
	// runs, returning "pg:name" → dump file.
	Backup func(ctx context.Context, app *state.App, rel *state.Release, out io.Writer) (map[string]string, error)
	// RestoreData loads those dumps back (rollback --with-data).
	RestoreData func(ctx context.Context, app *state.App, backups map[string]string, out io.Writer) error

	HealthInterval time.Duration
	MinFreeBytes   uint64
	// PortFree reports whether a host port can be bound; overridable in tests.
	PortFree func(port int) bool

	mu       sync.Mutex
	appLocks map[string]*sync.Mutex
	portMu   sync.Mutex
	busy     map[string]bool
	draining map[int]*drainer // host port → old container finishing its drain
}

func New(store *state.Store, rt runtime.Runtime, router Router) *Deployer {
	return &Deployer{
		Store:          store,
		RT:             rt,
		Router:         router,
		GitAuth:        func(*state.App) gitx.Auth { return gitx.Auth{} },
		Event:          func(state.Event) {},
		HealthInterval: 500 * time.Millisecond,
		MinFreeBytes:   2 << 30,
		PortFree:       portFree,
		appLocks:       map[string]*sync.Mutex{},
		busy:           map[string]bool{},
		draining:       map[int]*drainer{},
	}
}

// Lock serialises operations on one app. Deploys of different apps run
// concurrently.
func (d *Deployer) Lock(app string) func() {
	d.mu.Lock()
	l, ok := d.appLocks[app]
	if !ok {
		l = &sync.Mutex{}
		d.appLocks[app] = l
	}
	d.mu.Unlock()
	l.Lock()
	d.mu.Lock()
	d.busy[app] = true
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.busy[app] = false
		d.mu.Unlock()
		l.Unlock()
	}
}

// Busy reports whether an operation is in flight for app (the supervisor
// leaves busy apps alone).
func (d *Deployer) Busy(app string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busy[app]
}

// AnyBusy reports whether any deploy is in flight.
func (d *Deployer) AnyBusy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.busy {
		if b {
			return true
		}
	}
	return false
}

type Request struct {
	App     string
	Ref     string // branch, sha or empty for the app's branch
	Trigger string // webhook, poll, push, cli, config, restore
	NoCache bool
	// ReuseImage deploys the current release's image with fresh config
	// (config:set, pg:link) instead of building.
	ReuseImage bool
	Out        io.Writer
}

// ErrNothingToDeploy is returned for a ReuseImage request on an app that has
// never been deployed.
var ErrNothingToDeploy = errors.New("app has no release yet")

func say(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, "-----> "+format+"\n", args...)
	}
}

func detail(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, "       "+format+"\n", args...)
	}
}

// Deploy builds (or reuses) an image, starts it, health-checks it and swaps
// traffic to it.
func (d *Deployer) Deploy(ctx context.Context, req Request) (*state.Release, error) {
	unlock := d.Lock(req.App)
	defer unlock()
	out := req.Out
	if out == nil {
		out = io.Discard
	}
	app, err := d.Store.App(req.App)
	if err != nil {
		return nil, err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return nil, err
	}
	if err := d.checkDisk(); err != nil {
		return nil, err
	}
	env, err := d.Store.Env(app.Name)
	if err != nil {
		return nil, err
	}
	n := d.Store.NextReleaseNumber(app.Name)
	rel := &state.Release{N: n, Trigger: req.Trigger, CreatedAt: time.Now(), Status: state.StatusBuilding}

	if req.ReuseImage {
		cur := d.Store.CurrentRelease(app.Name)
		if cur == nil {
			return nil, ErrNothingToDeploy
		}
		rel.SHA, rel.Image, rel.Message, rel.ProcfileRelease = cur.SHA, cur.Image, cur.Message, cur.ProcfileRelease
		rel.Processes, rel.Cron = cur.Processes, cur.Cron
		say(out, "Releasing %s v%d (config change, image from v%d)", app.Name, n, cur.N)
	} else {
		start := time.Now()
		sha, subject, buildDir, err := d.checkout(ctx, app, req.Ref, n, out)
		if err != nil {
			d.Event(state.Event{Kind: "deploy.failed", App: app.Name, Message: err.Error()})
			return nil, err
		}
		defer os.RemoveAll(buildDir)
		rel.SHA, rel.Message, rel.Image = sha, subject, state.ImageName(app.Name, n)
		rel.ProcfileRelease = detect.ProcfileRelease(buildDir)
		if err := readProcesses(buildDir, rel, out); err != nil {
			d.Event(state.Event{Kind: "deploy.failed", App: app.Name, Message: err.Error()})
			return nil, err
		}
		d.Event(state.Event{Kind: "deploy.start", App: app.Name, Message: fmt.Sprintf("v%d %s@%s (%s)", n, app.Branch, short(sha), req.Trigger)})
		if err := d.Store.SaveRelease(app.Name, rel); err != nil {
			return nil, err
		}
		if err := d.build(ctx, app, env, rel, buildDir, req.NoCache, out); err != nil {
			if NetworkFailure(d.Store.Path("apps", app.Name, "logs", fmt.Sprintf("build-%d.log", n))) {
				say(out, "The build couldn't reach the internet. Apple's container network sometimes")
				detail(out, "loses outbound access after the Mac's network changes. Fix it with")
				detail(out, "  container system stop && container system start")
				detail(out, "(running apps restart automatically), then deploy again. `berth doctor` checks this.")
				d.Event(state.Event{Kind: "network", App: app.Name, Message: "containers have no outbound network: run `container system stop && container system start`"})
				err = fmt.Errorf("containers can't reach the internet (see above)")
			}
			rel.Status, rel.Error, rel.FailedStage = state.StatusFailed, err.Error(), "build"
			d.Store.SaveRelease(app.Name, rel)
			d.Event(state.Event{Kind: "deploy.failed", App: app.Name, Message: fmt.Sprintf("v%d build failed: %v", n, err)})
			return rel, err
		}
		rel.BuildSecs = time.Since(start).Seconds()
		if env.StrSetting(state.KeyRelease, rel.ProcfileRelease) == "" {
			if tool, cmd := detect.MigrationHint(buildDir); tool != "" {
				say(out, "This looks like %s. To run migrations before each release, add a Procfile line", tool)
				detail(out, "  release: %s", cmd)
				detail(out, "or `berth config:set %s %s=%q`", app.Name, state.KeyRelease, cmd)
			}
		}
	}
	rel.Env = ContainerEnv(env)
	rel.ReleaseCmd = env.StrSetting(state.KeyRelease, rel.ProcfileRelease)
	if err := d.release(ctx, app, cfg, rel, env, !req.ReuseImage, out); err != nil {
		rel.Status, rel.Error, rel.FailedStage = state.StatusFailed, err.Error(), "release"
		d.Store.SaveRelease(app.Name, rel)
		d.Event(state.Event{Kind: "deploy.failed", App: app.Name, Message: fmt.Sprintf("v%d: %v", n, err)})
		if prev := d.Store.CurrentRelease(app.Name); prev != nil && prev.Status == state.StatusRunning {
			detail(out, "v%d is still serving traffic", prev.N)
		}
		for svc, f := range rel.Backups {
			name := strings.TrimPrefix(svc, "pg:")
			detail(out, "if it changed %s before failing, the backup from just before is", svc)
			detail(out, "  berth pg:restore %s %s", name, f)
		}
		return rel, err
	}
	if err := d.startCheckSwap(ctx, app, cfg, rel, env, out); err != nil {
		d.Event(state.Event{Kind: "deploy.failed", App: app.Name, Message: fmt.Sprintf("v%d: %v", n, err)})
		return rel, err
	}
	d.Event(state.Event{Kind: "deploy.ok", App: app.Name, Message: fmt.Sprintf("v%d %s live", n, short(rel.SHA)), Fields: map[string]any{"release": n, "sha": rel.SHA}})
	d.gc(ctx, app, env, cfg, out)
	return rel, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// checkout fetches and exports the commit into a fresh build directory.
func (d *Deployer) checkout(ctx context.Context, app *state.App, ref string, n int, out io.Writer) (sha, subject, dir string, err error) {
	repo := d.Store.Path("apps", app.Name, "repo.git")
	if app.DevOf != "" {
		// A dev version builds snapshots pushed to its parent's mirror.
		repo = d.Store.Path("apps", app.DevOf, "repo.git")
	}
	url := ""
	if app.Repo != "" {
		url = app.CloneURL()
	}
	if err := gitx.EnsureMirror(ctx, repo, url); err != nil {
		return "", "", "", err
	}
	auth := d.GitAuth(app)
	if ref == "" {
		ref = app.Branch
	}
	if url != "" {
		say(out, "Fetching %s (%s)", app.Repo, app.Branch)
		if _, ferr := gitx.Fetch(ctx, repo, app.Branch, auth); ferr != nil {
			// A ref pushed straight to the Mac can still deploy without origin.
			if _, rerr := gitx.Resolve(ctx, repo, ref); rerr != nil {
				return "", "", "", ferr
			}
			detail(out, "fetch failed (%v); using local copy", ferr)
		}
	}
	sha, err = gitx.Resolve(ctx, repo, ref)
	if err != nil && url != "" && isHex(ref) {
		if gitx.FetchSHA(ctx, repo, ref, auth) == nil {
			sha, err = gitx.Resolve(ctx, repo, ref)
		}
	}
	if err != nil {
		return "", "", "", err
	}
	subject = gitx.Subject(ctx, repo, sha)
	dir = d.Store.Path("apps", app.Name, "build", fmt.Sprintf("%s-%d", short(sha), n))
	os.RemoveAll(dir)
	if err := gitx.Export(ctx, repo, sha, app.Path, dir); err != nil {
		os.RemoveAll(dir)
		return "", "", "", err
	}
	return sha, subject, dir, nil
}

func isHex(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// networkSignatures are what package managers print when a container has no
// outbound network.
var networkSignatures = []string{
	"Network is unreachable", "network is unreachable", "Temporary failure in name resolution",
	"Could not resolve host", "getaddrinfo EAI_AGAIN", "ETIMEDOUT", "ENETUNREACH",
	"error sending request for url", "Failed to establish a new connection", "dial tcp: lookup",
}

// NetworkFailure reports whether a build log looks like the container had
// no outbound network (as opposed to a broken Dockerfile).
func NetworkFailure(logPath string) bool {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}
	s := string(b)
	for _, sig := range networkSignatures {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// ResolveDockerfile returns the Dockerfile path to build with, rendering a
// template into the build dir when the repo has none.
func ResolveDockerfile(dir string, env state.Env) (path, stack string, err error) {
	if p := env.StrSetting(state.KeyDockerfile, ""); p != "" {
		full := filepath.Join(dir, filepath.Clean("/"+p))
		if _, err := os.Stat(full); err != nil {
			return "", "", fmt.Errorf("%s=%s: not found in repo", state.KeyDockerfile, p)
		}
		return full, "dockerfile", nil
	}
	for _, name := range []string{"Dockerfile", "Containerfile"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name), "dockerfile", nil
		}
	}
	res, err := detect.Detect(dir)
	if err != nil {
		return "", "", err
	}
	p := filepath.Join(dir, ".berth.Dockerfile")
	if err := os.WriteFile(p, []byte(res.Dockerfile), 0o644); err != nil {
		return "", "", err
	}
	return p, res.Stack, nil
}

func (d *Deployer) build(ctx context.Context, app *state.App, env state.Env, rel *state.Release, dir string, noCache bool, out io.Writer) error {
	dockerfile, stack, err := ResolveDockerfile(dir, env)
	if err != nil {
		return err
	}
	if stack == "dockerfile" {
		relPath, _ := filepath.Rel(dir, dockerfile)
		say(out, "Building %s v%d from %s@%s using %s", app.Name, rel.N, app.Branch, short(rel.SHA), relPath)
	} else {
		say(out, "Building %s v%d from %s@%s (detected %s; `berth dockerfile %s` prints it)", app.Name, rel.N, app.Branch, short(rel.SHA), stack, app.Name)
	}
	logPath := d.Store.Path("apps", app.Name, "logs", fmt.Sprintf("build-%d.log", rel.N))
	os.MkdirAll(filepath.Dir(logPath), 0o700)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	start := time.Now()
	err = d.RT.Build(ctx, runtime.BuildOpts{
		Tag:        rel.Image,
		Dockerfile: dockerfile,
		Context:    dir,
		BuildArgs:  buildArgs(env),
		NoCache:    noCache,
		Out:        io.MultiWriter(&capWriter{w: lf, left: maxBuildLog}, indent(out)),
	})
	if err != nil {
		return err
	}
	say(out, "Built %s in %.0fs", rel.Image, time.Since(start).Seconds())
	return nil
}

// buildArgs passes only variables the user marked with
// `config:set --build-arg` (listed in BERTH_BUILD_ARGS).
func buildArgs(env state.Env) []string {
	var args []string
	for _, k := range strings.Split(env["BERTH_BUILD_ARGS"], ",") {
		k = strings.TrimSpace(k)
		if v, ok := env[k]; ok && k != "" {
			args = append(args, k+"="+v)
		}
	}
	return args
}

// ContainerEnv is what the app sees: everything except BERTH_* settings,
// with PORT defaulted to 8080.
func ContainerEnv(env state.Env) map[string]string {
	m := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "BERTH_") {
			continue
		}
		m[k] = v
	}
	if _, ok := m["PORT"]; !ok {
		m["PORT"] = "8080"
	}
	return m
}

// startCheckSwap runs rel in a new container, waits for it to be healthy,
// then points traffic at it and retires the previous release.
func (d *Deployer) startCheckSwap(ctx context.Context, app *state.App, cfg *state.Config, rel *state.Release, env state.Env, out io.Writer) error {
	prev := d.Store.CurrentRelease(app.Name)
	avoid := 0
	if prev != nil && prev.Container != "" {
		avoid = prev.HostPort
	}
	port, err := d.pickSlot(app, cfg, avoid)
	if err != nil {
		return err
	}
	rel.HostPort = port
	rel.Status = state.StatusStarting
	rel.Container = containerName(app.Name, rel.N)
	if err := d.Store.SaveRelease(app.Name, rel); err != nil {
		return err
	}
	say(out, "Starting %s on 127.0.0.1:%d", rel.Container, port)
	if err := d.runContainer(ctx, app, cfg, rel, env); err != nil {
		rel.Status, rel.Error, rel.FailedStage = state.StatusFailed, err.Error(), "start"
		d.Store.SaveRelease(app.Name, rel)
		return err
	}
	if err := d.waitHealthy(ctx, rel, env, out); err != nil {
		logs := d.lastLogs(ctx, rel.Container, 50)
		d.RT.Remove(context.Background(), rel.Container)
		rel.Status, rel.Error, rel.FailedStage = state.StatusFailed, err.Error(), "start"
		d.Store.SaveRelease(app.Name, rel)
		if logs != "" {
			fmt.Fprintf(out, "-----> Last log lines from %s:\n%s", rel.Container, indentString(logs))
		}
		if prev != nil && prev.Status == state.StatusRunning {
			detail(out, "v%d is still serving traffic", prev.N)
		}
		return err
	}
	rel.Status, rel.Error, rel.StartedAt = state.StatusRunning, "", time.Now()
	if err := d.Store.SaveRelease(app.Name, rel); err != nil {
		return err
	}
	if err := d.Store.SetCurrent(app.Name, rel.N); err != nil {
		return err
	}
	if app.Stopped {
		app.Stopped = false
		d.Store.SaveApp(app)
	}
	if d.Router != nil {
		if err := d.Router.Apply(ctx); err != nil {
			detail(out, "warning: proxy update failed: %v", err)
		}
	}
	// Workers move straight over (stopped before starting the new ones, so
	// no job is picked up twice); retire saves prev after its drain, so
	// this saves first.
	if prev != nil && prev.N != rel.N {
		d.stopWorkers(ctx, app.Name, prev, out)
	}
	if prev != nil && prev.N != rel.N && prev.Container != "" {
		drain := env.IntSetting(state.KeyDrain, 5)
		say(out, "v%d is live; v%d drains for %ds, then stops", rel.N, prev.N, drain)
		go d.retire(app.Name, prev.N, prev.Container, prev.HostPort, time.Duration(drain)*time.Second)
	}
	d.startWorkers(ctx, app, cfg, rel, env, out)
	return nil
}

// retire stops and removes an old container after the drain period. It
// runs in the background so the CLI returns as soon as traffic has moved.
//
// While it drains, the container still holds its slot; a deploy that needs
// the slot ends the drain early (see pickSlot).
func (d *Deployer) retire(app string, n int, container string, port int, drain time.Duration) {
	dr := &drainer{early: make(chan struct{}), done: make(chan struct{})}
	d.mu.Lock()
	d.draining[port] = dr
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.draining[port] == dr {
			delete(d.draining, port)
		}
		d.mu.Unlock()
		close(dr.done)
	}()
	select {
	case <-time.After(drain):
	case <-dr.early:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	d.RT.Stop(ctx, container, 10*time.Second)
	d.RT.Remove(ctx, container)
	if r, err := d.Store.Release(app, n); err == nil && r.Container == container && d.Store.Current(app) != n {
		r.Status = state.StatusStopped
		r.Container = ""
		d.Store.SaveRelease(app, r)
	}
}

type drainer struct {
	early chan struct{}
	done  chan struct{}
	once  sync.Once
}

// endDrain stops a draining container on port now, returning once the
// port is released (or after a timeout).
func (d *Deployer) endDrain(port int) bool {
	d.mu.Lock()
	dr := d.draining[port]
	d.mu.Unlock()
	if dr == nil {
		return false
	}
	dr.once.Do(func() { close(dr.early) })
	select {
	case <-dr.done:
	case <-time.After(30 * time.Second):
	}
	return true
}

// containerName is unique per start, so a release can be restarted
// side-by-side with its own running container (zero-downtime restart).
func containerName(app string, n int) string {
	return fmt.Sprintf("%s-%s", state.ContainerName(app, n), strconv.FormatInt(time.Now().UnixMilli()%1_000_000, 36))
}

func (d *Deployer) runContainer(ctx context.Context, app *state.App, cfg *state.Config, rel *state.Release, env state.Env) error {
	envFile := d.Store.Path("apps", app.Name, "releases", fmt.Sprintf("%d.env", rel.N))
	file, extra := state.Env(rel.Env).ContainerEnvFile()
	if err := state.WriteFile(envFile, []byte(file), 0o600); err != nil {
		return err
	}
	containerPort := rel.Env["PORT"]
	var vols []string
	for _, m := range app.Mounts {
		if err := os.MkdirAll(m.Source, 0o755); err != nil {
			return err
		}
		vols = append(vols, m.Source+":"+m.Path)
	}
	d.RT.Remove(ctx, rel.Container) // stale container with the same name
	return d.RT.Run(ctx, runtime.RunOpts{
		Name:    rel.Container,
		Image:   rel.Image,
		Network: cfg.Network,
		Publish: []string{fmt.Sprintf("127.0.0.1:%d:%s", rel.HostPort, containerPort)},
		EnvFile: envFile,
		Env:     extra,
		CPUs:    env.StrSetting(state.KeyCPUs, cfg.DefaultCPUs),
		Memory:  env.StrSetting(state.KeyMemory, cfg.DefaultMemory),
		Volumes: vols,
		Labels:  map[string]string{"dev.berth.app": app.Name, "dev.berth.release": strconv.Itoa(rel.N)},
		Init:    true,
		Detach:  true,
	})
}

// errorLine picks the line of a failed command's output most likely to
// say what went wrong: the last one mentioning an error, else the last.
func errorLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.Contains(strings.ToLower(l), "error") || strings.Contains(l, "FATAL") {
			return l
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// release runs the release command (migrations) in a one-off container
// from the new image with the new release's config, before the new
// release takes traffic. If it fails, the deploy stops and the current
// release keeps serving. On production, a new build first gets a backup of
// the app's Postgres databases (config-only releases rerun commands that
// have already run, so they skip it).
func (d *Deployer) release(ctx context.Context, app *state.App, cfg *state.Config, rel *state.Release, env state.Env, backup bool, out io.Writer) error {
	if rel.ReleaseCmd == "" {
		return nil
	}
	if backup && app.DevOf == "" && d.Backup != nil && env.StrSetting(state.KeyReleaseBackup, "on") != "off" {
		b, err := d.Backup(ctx, app, rel, out)
		if err != nil {
			return fmt.Errorf("backup before the release command failed: %w (BERTH_RELEASE_BACKUP=off skips it)", err)
		}
		rel.Backups, rel.DataOf = b, d.Store.Current(app.Name)
	}
	rel.Status = state.StatusReleasing
	if err := d.Store.SaveRelease(app.Name, rel); err != nil {
		return err
	}
	timeout := time.Duration(env.IntSetting(state.KeyReleaseTimeout, 600)) * time.Second
	say(out, "Running release command: %s", rel.ReleaseCmd)
	logPath := d.Store.Path("apps", app.Name, "logs", fmt.Sprintf("release-%d.log", rel.N))
	os.MkdirAll(filepath.Dir(logPath), 0o700)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	envFile := d.Store.Path("apps", app.Name, "releases", fmt.Sprintf("%d.env", rel.N))
	file, extra := state.Env(rel.Env).ContainerEnvFile()
	if err := state.WriteFile(envFile, []byte(file), 0o600); err != nil {
		return err
	}
	var vols []string
	for _, m := range app.Mounts {
		if err := os.MkdirAll(m.Source, 0o755); err != nil {
			return err
		}
		vols = append(vols, m.Source+":"+m.Path)
	}
	name := state.ContainerName(app.Name, rel.N) + "-release"
	d.RT.Remove(ctx, name)
	tail := &tailWriter{max: 8 << 10}
	w := io.MultiWriter(&capWriter{w: lf, left: maxBuildLog}, indent(out), tail)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	err = d.RT.RunWith(rctx, runtime.RunOpts{
		Name:       name,
		Image:      rel.Image,
		Network:    cfg.Network,
		EnvFile:    envFile,
		Env:        extra,
		CPUs:       env.StrSetting(state.KeyCPUs, cfg.DefaultCPUs),
		Memory:     env.StrSetting(state.KeyMemory, cfg.DefaultMemory),
		Volumes:    vols,
		Labels:     map[string]string{"dev.berth.app": app.Name, "dev.berth.release": strconv.Itoa(rel.N), "dev.berth.role": "release"},
		Init:       true,
		Remove:     true,
		Entrypoint: "/bin/sh",
		Args:       []string{"-c", rel.ReleaseCmd},
		Stderr:     w,
	}, nil, w)
	rel.ReleaseSecs = time.Since(start).Seconds()
	if err != nil {
		// A timeout kills the CLI, not necessarily the container.
		d.RT.Remove(context.Background(), name)
		if rctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("release command timed out after %s (%s raises it)", timeout, state.KeyReleaseTimeout)
		}
		if line := errorLine(string(tail.b)); line != "" {
			return fmt.Errorf("release command failed: %s", line)
		}
		return fmt.Errorf("release command failed: %v", err)
	}
	say(out, "Release command finished in %.1fs", rel.ReleaseSecs)
	return nil
}

func (d *Deployer) waitHealthy(ctx context.Context, rel *state.Release, env state.Env, out io.Writer) error {
	path := env.StrSetting(state.KeyHealthcheck, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	timeout := time.Duration(env.IntSetting(state.KeyHealthTimeout, 60)) * time.Second
	url := fmt.Sprintf("http://127.0.0.1:%d%s", rel.HostPort, path)
	say(out, "Waiting for %s to answer on %s (up to %s)", rel.Container, path, timeout)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	start := time.Now()
	lastErr := ""
	checks := 0
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("User-Agent", "berth-healthcheck")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				say(out, "Healthy in %.1fs (HTTP %d)", time.Since(start).Seconds(), resp.StatusCode)
				return nil
			}
			lastErr = "HTTP " + resp.Status
		} else {
			lastErr = err.Error()
		}
		checks++
		// Fail fast if the container has already exited.
		if checks%4 == 0 {
			if cs, err := d.RT.List(ctx); err == nil {
				if c, ok := runtime.Find(cs, rel.Container); !ok || !c.Running() {
					return fmt.Errorf("container exited during startup")
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.HealthInterval):
		}
	}
	return fmt.Errorf("health check %s failed after %s: %s", path, timeout, lastErr)
}

func (d *Deployer) lastLogs(ctx context.Context, container string, n int) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var b bytes.Buffer
	d.RT.Logs(ctx, container, false, n, &b)
	return b.String()
}

// pickSlot returns the host port for a new container of app. Each app owns
// two fixed ports and releases alternate between them, so Caddy's route
// (both ports, "first available" with retries) never has to change during
// a deploy: no config reload, so no dropped connections. avoid is the port
// of the container being replaced.
func (d *Deployer) pickSlot(app *state.App, cfg *state.Config, avoid int) (int, error) {
	d.portMu.Lock()
	defer d.portMu.Unlock()
	if len(app.Slots) != 2 {
		slots, err := d.allocateSlots(cfg)
		if err != nil {
			return 0, err
		}
		app.Slots = slots
		if err := d.Store.SaveApp(app); err != nil {
			return 0, err
		}
		if d.Router != nil {
			d.Router.Apply(context.Background())
		}
	}
	for _, p := range app.Slots {
		if p != avoid && d.PortFree(p) {
			return p, nil
		}
	}
	// A release still draining from an earlier deploy may hold the slot we
	// need: it is no longer current, so finish its drain now.
	for _, p := range app.Slots {
		if p != avoid && d.endDrain(p) && d.PortFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("both of %s's ports (%d, %d) are in use; `berth restart %s` or check `lsof -i :%d`", app.Name, app.Slots[0], app.Slots[1], app.Name, app.Slots[0])
}

// allocateSlots finds two ports no app or running container uses.
func (d *Deployer) allocateSlots(cfg *state.Config) ([]int, error) {
	used := map[int]bool{}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		for _, p := range a.Slots {
			used[p] = true
		}
		rs, _ := d.Store.Releases(a.Name)
		for _, r := range rs {
			if r.Container != "" || r.Status == state.StatusRunning || r.Status == state.StatusStarting {
				used[r.HostPort] = true
			}
		}
	}
	var slots []int
	for p := cfg.PortMin; p <= cfg.PortMax && len(slots) < 2; p++ {
		if !used[p] && d.PortFree(p) {
			slots = append(slots, p)
		}
	}
	if len(slots) < 2 {
		return nil, fmt.Errorf("no free host ports left in %d-%d", cfg.PortMin, cfg.PortMax)
	}
	return slots, nil
}

func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func (d *Deployer) checkDisk() error {
	free, err := FreeBytes(d.Store.Root)
	if err != nil {
		return nil
	}
	if free < d.MinFreeBytes {
		return fmt.Errorf("only %s free on disk; refusing to deploy below %s (try `berth gc`)", HumanBytes(int64(free)), HumanBytes(int64(d.MinFreeBytes)))
	}
	return nil
}

func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Rollback swaps back to release n, or to the newest release before the
// current one that once ran successfully. withData also restores the
// databases from the backup taken while that release was current (just
// before a later release's command ran), after the swap: old code copes
// with a newer schema better than new code with an older one.
func (d *Deployer) Rollback(ctx context.Context, appName string, n int, withData bool, out io.Writer) (*state.Release, error) {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil {
		return nil, err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return nil, err
	}
	cur := d.Store.Current(appName)
	target, err := d.RollbackTarget(appName, n)
	if err != nil {
		return nil, err
	}
	var data *state.Release
	if withData {
		if data, err = d.DataFor(appName, target.N); err != nil {
			return nil, err
		}
	}
	env, _ := d.Store.Env(appName)
	say(out, "Rolling back %s to v%d (%s)", appName, target.N, short(target.SHA))
	if err := d.startCheckSwap(ctx, app, cfg, target, env, out); err != nil {
		return target, err
	}
	msg := fmt.Sprintf("v%d → v%d", cur, target.N)
	if data != nil {
		say(out, "Restoring the data as it was before v%d's release command (%s)", data.N, data.CreatedAt.Format("2006-01-02 15:04"))
		if err := d.RestoreData(ctx, app, data.Backups, out); err != nil {
			return target, fmt.Errorf("v%d is live but restoring its data failed: %w", target.N, err)
		}
		msg += fmt.Sprintf(" with data from before v%d", data.N)
	}
	d.Event(state.Event{Kind: "rollback", App: appName, Message: msg})
	return target, nil
}

// RollbackTarget is the release `berth rollback app [n]` would swap to.
func (d *Deployer) RollbackTarget(appName string, n int) (*state.Release, error) {
	cur := d.Store.Current(appName)
	rs, _ := d.Store.Releases(appName)
	var target *state.Release
	for _, r := range rs {
		if n != 0 {
			if r.N == n {
				target = r
				break
			}
			continue
		}
		if r.N < cur && !r.StartedAt.IsZero() && r.Status != state.StatusFailed && !r.Pruned {
			target = r
			break
		}
	}
	if target == nil {
		if n != 0 {
			return nil, fmt.Errorf("no release v%d", n)
		}
		return nil, fmt.Errorf("no earlier successful release to roll back to")
	}
	if target.N == cur {
		return nil, fmt.Errorf("v%d is already current", cur)
	}
	if target.Pruned {
		return nil, fmt.Errorf("v%d's image was garbage-collected; redeploy its commit with `berth deploy %s --ref %s`", target.N, appName, short(target.SHA))
	}
	if target.Env == nil {
		return nil, fmt.Errorf("v%d has no config snapshot", target.N)
	}
	return target, nil
}

// DataFor returns the newest release whose pre-release backups hold the
// data as release n left it.
func (d *Deployer) DataFor(appName string, n int) (*state.Release, error) {
	if d.RestoreData == nil {
		return nil, fmt.Errorf("restoring data isn't available")
	}
	rs, _ := d.Store.Releases(appName) // newest first
	for _, r := range rs {
		if r.DataOf != n || len(r.Backups) == 0 {
			continue
		}
		for svc, f := range r.Backups {
			if _, err := os.Stat(f); err != nil {
				return nil, fmt.Errorf("v%d's backup of %s is gone (%s)", r.N, svc, f)
			}
		}
		return r, nil
	}
	return nil, fmt.Errorf("no backup holds v%d's data: no release command ran while v%d was live, so a plain rollback (without --with-data) is enough", n, n)
}

// Restart starts a fresh container for the current release and swaps to it.
func (d *Deployer) Restart(ctx context.Context, appName string, out io.Writer) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	rel := d.Store.CurrentRelease(appName)
	if rel == nil {
		return ErrNothingToDeploy
	}
	env, _ := d.Store.Env(appName)
	old, oldPort := rel.Container, rel.HostPort
	say(out, "Restarting %s v%d", appName, rel.N)
	// startCheckSwap retires "prev", which is this same release; handle the
	// old container ourselves.
	fresh := *rel
	if err := d.startCheckSwapSame(ctx, app, cfg, &fresh, env, out); err != nil {
		return err
	}
	if old != "" && old != fresh.Container {
		go d.retire(appName, rel.N, old, oldPort, time.Duration(env.IntSetting(state.KeyDrain, 5))*time.Second)
	}
	d.stopWorkers(ctx, appName, &fresh, out)
	d.startWorkers(ctx, app, cfg, &fresh, env, out)
	d.Event(state.Event{Kind: "restart", App: appName, Message: fmt.Sprintf("v%d restarted", rel.N)})
	return nil
}

func (d *Deployer) startCheckSwapSame(ctx context.Context, app *state.App, cfg *state.Config, rel *state.Release, env state.Env, out io.Writer) error {
	avoid := 0
	if rel.Container != "" {
		avoid = rel.HostPort
	}
	port, err := d.pickSlot(app, cfg, avoid)
	if err != nil {
		return err
	}
	rel.HostPort = port
	rel.Container = containerName(app.Name, rel.N)
	if err := d.runContainer(ctx, app, cfg, rel, env); err != nil {
		return err
	}
	if err := d.waitHealthy(ctx, rel, env, out); err != nil {
		logs := d.lastLogs(ctx, rel.Container, 50)
		d.RT.Remove(context.Background(), rel.Container)
		if logs != "" {
			fmt.Fprintf(out, "-----> Last log lines:\n%s", indentString(logs))
		}
		return err
	}
	rel.Status, rel.StartedAt = state.StatusRunning, time.Now()
	if err := d.Store.SaveRelease(app.Name, rel); err != nil {
		return err
	}
	if app.Stopped {
		app.Stopped = false
		d.Store.SaveApp(app)
	}
	if d.Router != nil {
		if err := d.Router.Apply(ctx); err != nil {
			detail(out, "warning: proxy update failed: %v", err)
		}
	}
	return nil
}

// Stop stops the app's container; the route answers 503 until start.
func (d *Deployer) Stop(ctx context.Context, appName string) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	app.Stopped = true
	if err := d.Store.SaveApp(app); err != nil {
		return err
	}
	if d.Router != nil {
		d.Router.Apply(ctx)
	}
	if rel := d.Store.CurrentRelease(appName); rel != nil {
		d.stopWorkers(ctx, appName, rel, nil)
		if rel.Container != "" {
			d.RT.Stop(ctx, rel.Container, 10*time.Second)
			d.RT.Remove(ctx, rel.Container)
			rel.Container = ""
			rel.Status = state.StatusStopped
			d.Store.SaveRelease(appName, rel)
		}
	}
	d.Event(state.Event{Kind: "stop", App: appName, Message: "stopped"})
	return nil
}

// Start brings a stopped app's current release back.
func (d *Deployer) Start(ctx context.Context, appName string, out io.Writer) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	rel := d.Store.CurrentRelease(appName)
	if rel == nil {
		return ErrNothingToDeploy
	}
	env, _ := d.Store.Env(appName)
	app.Stopped = false
	d.Store.SaveApp(app)
	say(out, "Starting %s v%d", appName, rel.N)
	if err := d.startCheckSwapSame(ctx, app, cfg, rel, env, out); err != nil {
		return err
	}
	d.stopWorkers(ctx, appName, rel, nil) // leftovers, if any
	d.startWorkers(ctx, app, cfg, rel, env, out)
	d.Event(state.Event{Kind: "start", App: appName, Message: fmt.Sprintf("v%d started", rel.N)})
	return nil
}

// Recover (re)starts the current release if its container is not running.
// Used by the supervisor and on berthd start. It does not health-gate
// traffic (there is nothing else to serve), but it does wait for health so
// the backoff logic knows whether the start worked.
func (d *Deployer) Recover(ctx context.Context, appName string) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil || app.Stopped {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	rel := d.Store.CurrentRelease(appName)
	if rel == nil {
		return nil
	}
	env, _ := d.Store.Env(appName)
	old := rel.Container
	if old != "" {
		d.RT.Remove(ctx, old)
	}
	err = d.startCheckSwapSame(ctx, app, cfg, rel, env, io.Discard)
	if err != nil {
		rel.Container = ""
		rel.Status = state.StatusCrashed
		d.Store.SaveRelease(appName, rel)
		if d.Router != nil {
			d.Router.Apply(ctx)
		}
	}
	return err
}

// MarkCrashed records that the current release is down and will not be
// restarted (the supervisor gave up on a crash loop).
func (d *Deployer) MarkCrashed(appName string) {
	unlock := d.Lock(appName)
	defer unlock()
	rel := d.Store.CurrentRelease(appName)
	if rel == nil {
		return
	}
	if rel.Container != "" {
		d.RT.Remove(context.Background(), rel.Container)
	}
	rel.Container, rel.Status = "", state.StatusCrashed
	d.Store.SaveRelease(appName, rel)
}

// GC removes images and metadata for releases beyond the keep count.
func (d *Deployer) GC(ctx context.Context, appName string, out io.Writer) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	env, _ := d.Store.Env(appName)
	d.gc(ctx, app, env, cfg, out)
	return nil
}

func (d *Deployer) gc(ctx context.Context, app *state.App, env state.Env, cfg *state.Config, out io.Writer) {
	keep := env.IntSetting(state.KeyKeepReleases, cfg.KeepReleases)
	if keep < 1 {
		keep = 1
	}
	rs, _ := d.Store.Releases(app.Name)
	cur := d.Store.Current(app.Name)
	keepImages := map[string]bool{}
	var prune []*state.Release
	kept := 0
	for _, r := range rs { // newest first
		if r.Pruned {
			continue
		}
		if r.N == cur || kept < keep {
			if r.Status != state.StatusFailed {
				kept++
			}
			keepImages[r.Image] = true
			continue
		}
		prune = append(prune, r)
	}
	// Failed builds that never produced a running container are pruned too
	// once they fall outside the window.
	var refs []string
	for _, r := range prune {
		if r.Container != "" || len(r.Workers) > 0 {
			continue // still running (draining); next GC gets it
		}
		if !keepImages[r.Image] {
			refs = append(refs, r.Image)
			keepImages[r.Image] = true
		}
		r.Pruned = true
		r.Env = nil
		d.Store.SaveRelease(app.Name, r)
		os.Remove(d.Store.Path("apps", app.Name, "releases", fmt.Sprintf("%d.env", r.N)))
	}
	if len(refs) > 0 {
		imgs, _ := d.RT.Images(ctx)
		have := map[string]bool{}
		for _, i := range imgs {
			have[i.Name] = true
		}
		var existing []string
		for _, r := range refs {
			if have[r] {
				existing = append(existing, r)
			}
		}
		if len(existing) > 0 {
			if err := d.RT.RemoveImage(ctx, existing...); err == nil && out != nil {
				detail(out, "gc: removed %d old image(s)", len(existing))
			}
		}
	}
	// Old build and release-command logs follow the same window.
	for _, r := range prune {
		for _, kind := range []string{"build", "release"} {
			os.Remove(d.Store.Path("apps", app.Name, "logs", fmt.Sprintf("%s-%d.log", kind, r.N)))
		}
	}
}

const maxBuildLog = 50 << 20

// tailWriter keeps the last max bytes written.
type tailWriter struct {
	b   []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

// capWriter stops writing after a limit (build logs), noting the cut once.
type capWriter struct {
	w    io.Writer
	left int64
	cut  bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		if !c.cut {
			c.cut = true
			io.WriteString(c.w, "\n[berth: build log truncated at 50 MB]\n")
		}
		return len(p), nil
	}
	n := len(p)
	if int64(n) > c.left {
		p = p[:c.left]
	}
	m, err := c.w.Write(p)
	c.left -= int64(m)
	return n, err
}

type indentWriter struct {
	w   io.Writer
	bol bool
}

func indent(w io.Writer) io.Writer { return &indentWriter{w: w, bol: true} }

func (iw *indentWriter) Write(p []byte) (int, error) {
	var b bytes.Buffer
	for _, c := range p {
		if iw.bol {
			b.WriteString("       ")
			iw.bol = false
		}
		b.WriteByte(c)
		if c == '\n' {
			iw.bol = true
		}
	}
	_, err := iw.w.Write(b.Bytes())
	return len(p), err
}

func indentString(s string) string {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		b.WriteString("       " + sc.Text() + "\n")
	}
	return b.String()
}
