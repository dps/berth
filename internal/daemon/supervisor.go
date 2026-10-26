package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/gitx"
	"github.com/dps/berth/internal/offsite"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// crashState tracks restart backoff per app: 1 s doubling to a 60 s cap,
// reset after 10 minutes healthy; after 10 straight failures the app is
// left crashed until the next deploy or restart.
type crashState struct {
	failures  int
	next      time.Time
	healthy   time.Time // when the current run was first seen healthy
	container string
	gaveUp    bool
}

const (
	maxCrashFailures = 10
	crashResetAfter  = 10 * time.Minute
)

func backoff(failures int) time.Duration {
	d := time.Second << min(failures, 6)
	return min(d, time.Minute)
}

// ---- container network health ----

// netState tracks the outbound-network probe.
type netState struct {
	lastProbe time.Time
	failures  int
}

const (
	netProbeEvery  = 5 * time.Minute
	netRetryEvery  = time.Minute
	netFailsToHeal = 2
)

// ProbeOutbound runs a throwaway Alpine container on the berth network that
// fetches an HTTPS URL, which is what builds and most apps need.
func ProbeOutbound(ctx context.Context, rt runtime.Runtime, network string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return rt.RunWith(ctx, runtime.RunOpts{
		Image:   "alpine:3",
		Network: network,
		Remove:  true,
		Args:    []string{"wget", "-q", "-T", "8", "-O", "/dev/null", "https://cloudflare.com/cdn-cgi/trace"},
	}, nil, io.Discard)
}

// checkNetwork probes containers' outbound network every 5 minutes (each
// minute once it has failed) and, after two failures in a row, restarts
// Apple's container system, which has been seen to lose outbound NAT after
// the Mac's network changes. Running containers stop for a few seconds and
// are brought back by reconcile.
func (d *Daemon) checkNetwork(ctx context.Context) {
	cfg := d.cfg()
	every := netProbeEvery
	if d.net.failures > 0 {
		every = netRetryEvery
	}
	if time.Since(d.net.lastProbe) < every || d.Deployer.AnyBusy() {
		return
	}
	d.net.lastProbe = time.Now()
	err := ProbeOutbound(ctx, d.RT, cfg.Network)
	if err == nil {
		if d.net.failures > 0 {
			d.event(state.Event{Kind: "network", Message: "containers can reach the internet again"})
		}
		d.net.failures = 0
		return
	}
	d.net.failures++
	slog.Warn("containers have no outbound network", "failures", d.net.failures, "err", err)
	if d.net.failures < netFailsToHeal {
		return
	}
	if !cfg.HealNetwork() {
		if d.net.failures == netFailsToHeal {
			d.event(state.Event{Kind: "network", Message: "containers have no outbound network; run `container system stop && container system start` (auto-heal is off)"})
		}
		return
	}
	d.event(state.Event{Kind: "network", Message: "containers lost outbound network; restarting the container system (apps are down for a few seconds)"})
	d.healNetwork(ctx)
}

func (d *Daemon) healNetwork(ctx context.Context) {
	d.stopTails()
	if err := d.RT.SystemStop(ctx); err != nil {
		slog.Error("container system stop", "err", err)
	}
	if err := d.RT.SystemStart(ctx); err != nil {
		slog.Error("container system start", "err", err)
	}
	// The restart stopped every container on purpose; it is not a crash.
	d.mu.Lock()
	d.crash = map[string]*crashState{}
	d.mu.Unlock()
	d.reconcile(ctx)
	d.net.failures = 0
	d.net.lastProbe = time.Time{}
}

func (d *Daemon) superviseLoop(ctx context.Context) {
	t := time.NewTicker(d.SuperviseEvery)
	defer t.Stop()
	for {
		d.supervise(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Daemon) supervise(ctx context.Context) {
	if d.restoring.Load() {
		return
	}
	if d.selfUpdated() && !d.Deployer.AnyBusy() {
		d.reexec()
		return
	}
	d.sleepIdleDev(ctx)
	if d.tunnelRetry.Load() {
		if _, err := d.ApplyTunnel(ctx, true); err != nil {
			slog.Warn("tunnel config retry", "err", err)
		} else {
			d.event(state.Event{Kind: "tunnel", Message: "tunnel configuration applied after retry"})
		}
	}
	if !d.RT.SystemRunning(ctx) {
		slog.Warn("container system down; restarting it")
		if err := d.RT.SystemStart(ctx); err != nil {
			slog.Error("container system start", "err", err)
			return
		}
	}
	cs, err := d.RT.List(ctx)
	if err != nil {
		slog.Warn("container list", "err", err)
		return
	}
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		if started, err := d.Services.EnsureRunning(ctx, sv, cs); err != nil {
			slog.Error("service restart", "service", sv.Kind+":"+sv.Name, "err", err)
		} else if started {
			d.event(state.Event{Kind: "crash", Message: fmt.Sprintf("%s:%s was down; restarted", sv.Kind, sv.Name)})
		}
	}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		d.superviseApp(ctx, a, cs)
		d.superviseWorkers(ctx, a, cs)
	}
	d.syncTails(ctx, cs)
	d.checkNetwork(ctx)
	if d.mdns != nil {
		cfg := d.cfg()
		d.mdns.sync(cfg, apps)
	}
}

func (d *Daemon) superviseApp(ctx context.Context, a *state.App, cs []runtime.Container) {
	if a.Stopped || d.Deployer.Busy(a.Name) {
		return
	}
	rel := d.Store.CurrentRelease(a.Name)
	if rel == nil {
		return
	}
	d.mu.Lock()
	st, ok := d.crash[a.Name]
	if !ok {
		st = &crashState{}
		d.crash[a.Name] = st
	}
	d.mu.Unlock()

	if rel.Container != "" {
		if c, ok := runtime.Find(cs, rel.Container); ok && c.Running() {
			if st.container != rel.Container {
				// A deploy, restart or rollback happened: forget old failures.
				*st = crashState{container: rel.Container, healthy: time.Now()}
			}
			if st.failures > 0 && time.Since(st.healthy) > crashResetAfter {
				st.failures = 0
			}
			return
		}
	} else if rel.Status != state.StatusCrashed && rel.Status != state.StatusRunning {
		return // never started (stopped app, failed release)
	}
	// Stays given up until a deploy, rollback or `berth restart` brings up
	// a new container (the running branch above then resets the state).
	if st.gaveUp {
		return
	}
	if time.Now().Before(st.next) {
		return
	}
	// Every crash counts, whether or not the restart succeeds; only 10
	// healthy minutes reset the count.
	st.failures++
	if st.failures > maxCrashFailures {
		st.gaveUp = true
		st.container = ""
		d.Deployer.MarkCrashed(a.Name)
		if err := d.Apply(ctx); err != nil {
			slog.Warn("caddy apply", "err", err)
		}
		d.event(state.Event{Kind: "crash.loop", App: a.Name, Message: fmt.Sprintf("v%d crashed %d times in a row; Berth stopped restarting it until the next deploy or `berth restart %s` (see `berth logs %s`)", rel.N, maxCrashFailures, a.Name, a.Name)})
		return
	}
	d.event(state.Event{Kind: "crash", App: a.Name, Message: fmt.Sprintf("v%d is not running; restarting (crash %d of %d before Berth gives up)", rel.N, st.failures, maxCrashFailures)})
	st.next = time.Now().Add(d.CrashBackoff(st.failures))
	if err := d.Deployer.Recover(ctx, a.Name); err != nil {
		return
	}
	if r := d.Store.CurrentRelease(a.Name); r != nil {
		st.container = r.Container
	}
	st.healthy = time.Now()
}

// superviseWorkers restarts the current release's workers that have
// stopped, with the same backoff and give-up rule as the web process.
func (d *Daemon) superviseWorkers(ctx context.Context, a *state.App, cs []runtime.Container) {
	if a.Stopped || d.Deployer.Busy(a.Name) {
		return
	}
	rel := d.Store.CurrentRelease(a.Name)
	if rel == nil {
		return
	}
	for _, w := range rel.Workers {
		key := a.Name + "/" + w.Name()
		d.mu.Lock()
		st, ok := d.crash[key]
		if !ok {
			st = &crashState{}
			d.crash[key] = st
		}
		d.mu.Unlock()
		if c, ok := runtime.Find(cs, w.Container); ok && c.Running() {
			if st.container != w.Container {
				*st = crashState{container: w.Container, healthy: time.Now()}
			}
			if st.failures > 0 && time.Since(st.healthy) > crashResetAfter {
				st.failures = 0
			}
			continue
		}
		if st.gaveUp || time.Now().Before(st.next) {
			continue
		}
		st.failures++
		if st.failures > maxCrashFailures {
			st.gaveUp = true
			d.event(state.Event{Kind: "crash.loop", App: a.Name, Message: fmt.Sprintf("%s (v%d) exited %d times in a row; Berth stopped restarting it until the next deploy or `berth restart %s` (see `berth logs %s`)", w.Name(), rel.N, maxCrashFailures, a.Name, a.Name)})
			continue
		}
		d.event(state.Event{Kind: "crash", App: a.Name, Message: fmt.Sprintf("%s (v%d) is not running; restarting (%d of %d before Berth gives up)", w.Name(), rel.N, st.failures, maxCrashFailures)})
		st.next = time.Now().Add(d.CrashBackoff(st.failures))
		if err := d.Deployer.RecoverWorker(ctx, a.Name, w.Name()); err != nil {
			slog.Warn("worker restart", "app", a.Name, "worker", w.Name(), "err", err)
			continue
		}
		if r := d.Store.CurrentRelease(a.Name); r != nil {
			for _, nw := range r.Workers {
				if nw.Name() == w.Name() {
					st.container = nw.Container
				}
			}
		}
		st.healthy = time.Now()
	}
}

// ---- log tails ----

// syncTails keeps one `container logs --follow` per running app container,
// appending to apps/<app>/logs/app.log (rotated at 50 MB, 5 kept).
func (d *Daemon) syncTails(ctx context.Context, cs []runtime.Container) {
	d.mu.Lock()
	defer d.mu.Unlock()
	running := map[string]runtime.Container{}
	for _, c := range cs {
		// Cron jobs write their own log (logs --cron).
		if c.Running() && c.Labels["dev.berth.app"] != "" && c.Labels["dev.berth.role"] != "cron" {
			running[c.ID] = c
		}
	}
	for id, cancel := range d.tails {
		if _, ok := running[id]; !ok {
			cancel()
			delete(d.tails, id)
		}
	}
	for id, c := range running {
		if _, ok := d.tails[id]; ok {
			continue
		}
		tctx, cancel := context.WithCancel(ctx)
		d.tails[id] = cancel
		go d.tail(tctx, c.Labels["dev.berth.app"], c.Labels["dev.berth.release"], c.Labels["dev.berth.process"], id)
	}
}

func (d *Daemon) stopTails() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, cancel := range d.tails {
		cancel()
		delete(d.tails, id)
	}
}

// tailState remembers how many lines of each container's log were already
// copied, so a berthd restart does not duplicate them.
func (d *Daemon) tailOffsetPath(app, container string) string {
	return d.Store.Path("apps", app, "logs", ".tail-"+container)
}

// tail copies a container's output into app.log, each line stamped with
// the time and release, and the process for workers (v3 worker.1:).
func (d *Daemon) tail(ctx context.Context, app, release, process, container string) {
	defer func() {
		d.mu.Lock()
		delete(d.tails, container)
		d.mu.Unlock()
	}()
	w := NewRotatingWriter(d.Store.Path("apps", app, "logs", "app.log"), 50<<20, 5)
	defer w.Close()
	offPath := d.tailOffsetPath(app, container)
	var skip int
	if b, err := os.ReadFile(offPath); err == nil {
		fmt.Sscan(string(b), &skip)
	}
	pr, pw := io.Pipe()
	go func() {
		err := d.RT.Logs(ctx, container, true, -1, pw)
		pw.CloseWithError(err)
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	seen := 0
	lastSave := time.Now()
	for sc.Scan() {
		seen++
		if seen <= skip {
			continue
		}
		if process != "" {
			fmt.Fprintf(w, "%s v%s %s: %s\n", time.Now().UTC().Format(time.RFC3339), release, process, sc.Text())
		} else {
			fmt.Fprintf(w, "%s v%s: %s\n", time.Now().UTC().Format(time.RFC3339), release, sc.Text())
		}
		if time.Since(lastSave) > time.Second {
			os.WriteFile(offPath, []byte(fmt.Sprint(seen)), 0o600)
			lastSave = time.Now()
		}
	}
	os.WriteFile(offPath, []byte(fmt.Sprint(seen)), 0o600)
	// Container gone for good: drop the offset file.
	if cs, err := d.RT.List(context.Background()); err == nil {
		if _, ok := runtime.Find(cs, container); !ok {
			os.Remove(offPath)
		}
	}
}

// RotatingWriter appends to a file and rotates it at max bytes.
type RotatingWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func NewRotatingWriter(path string, max int64, keep int) *RotatingWriter {
	return &RotatingWriter{path: path, max: max, keep: keep}
}

func (r *RotatingWriter) open() error {
	os.MkdirAll(filepath.Dir(r.path), 0o700)
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, _ := f.Stat()
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *RotatingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	// Another tail may have rotated the file underneath us.
	if fi, err := os.Stat(r.path); err != nil || !os.SameFile(fi, mustStat(r.f)) {
		r.f.Close()
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > r.max {
		r.f.Close()
		for i := r.keep - 1; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		os.Rename(r.path, r.path+".1")
		os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep+1))
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func mustStat(f *os.File) os.FileInfo {
	fi, _ := f.Stat()
	return fi
}

func (r *RotatingWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// ---- polling ----

func (d *Daemon) pollLoop(ctx context.Context) {
	t := time.NewTicker(d.PollEvery)
	defer t.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		apps, _ := d.Store.Apps()
		// Pull requests are polled every fifth tick (5 minutes), behind
		// the webhook, to catch missed deliveries and closed PRs.
		if tick%5 == 0 {
			for _, a := range apps {
				if a.PreviewsOn {
					go func(name string) {
						if err := d.syncPreviews(ctx, name); err != nil {
							slog.Debug("previews", "app", name, "err", err)
						}
					}(a.Name)
				}
			}
		}
		for _, a := range apps {
			if a.Repo == "" || d.Deployer.Busy(a.Name) || d.isQueued(a.Name) {
				continue
			}
			if err := d.pollApp(ctx, a); err != nil {
				slog.Debug("poll", "app", a.Name, "err", err)
			}
		}
	}
}

// pollApp checks the branch head (GitHub API with ETag, or git ls-remote)
// and enqueues a deploy when it moved.
func (d *Daemon) pollApp(ctx context.Context, a *state.App) error {
	var head string
	// The GitHub API allows only 60 unauthenticated requests an hour, so
	// without a token GitHub repos are polled with git ls-remote instead.
	if a.IsGitHub() && !a.DeployKey && d.githubToken() != "" {
		sha, etag, notModified, err := d.GitHubAPI(d.githubToken()).BranchHead(ctx, a.Repo, a.Branch, a.PollETag)
		if err != nil {
			return err
		}
		if etag != a.PollETag {
			d.updateApp(a.Name, func(x *state.App) { x.PollETag = etag })
		}
		if notModified {
			return nil
		}
		head = sha
	} else {
		sha, err := gitx.LsRemote(ctx, a.CloneURL(), a.Branch, d.gitAuth(a))
		if err != nil {
			return err
		}
		head = sha
	}
	if head == "" || head == a.LastSeen {
		return nil
	}
	if cur := d.Store.CurrentRelease(a.Name); cur != nil && cur.SHA == head {
		d.updateApp(a.Name, func(x *state.App) { x.LastSeen = head })
		return nil
	}
	if a.Path != "" && a.LastSeen != "" {
		repo := d.Store.Path("apps", a.Name, "repo.git")
		if _, err := gitx.Fetch(ctx, repo, a.Branch, d.gitAuth(a)); err == nil && !gitx.Touches(ctx, repo, a.LastSeen, head, a.Path) {
			d.updateApp(a.Name, func(x *state.App) { x.LastSeen = head })
			return nil
		}
	}
	d.event(state.Event{Kind: "poll", App: a.Name, Message: fmt.Sprintf("%s moved to %s", a.Branch, head[:min(7, len(head))])})
	d.enqueue(a.Name, "poll", "")
	return nil
}

// updateApp applies a change to app.json under the app lock.
func (d *Daemon) updateApp(name string, f func(*state.App)) error {
	unlock := d.Deployer.Lock(name)
	defer unlock()
	a, err := d.Store.App(name)
	if err != nil {
		return err
	}
	f(a)
	return d.Store.SaveApp(a)
}

func (d *Daemon) isQueued(app string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queued[app]
}

// enqueue runs a background deploy, coalescing bursts: while one deploy
// is waiting, further triggers for the same app are absorbed (the waiting
// deploy fetches the branch head, so it picks up every push).
func (d *Daemon) enqueue(app, trigger, ref string) bool {
	d.mu.Lock()
	if d.queued[app] {
		d.mu.Unlock()
		return false
	}
	d.queued[app] = true
	d.mu.Unlock()
	go func() {
		for d.Deployer.Busy(app) {
			time.Sleep(200 * time.Millisecond)
		}
		d.mu.Lock()
		d.queued[app] = false
		d.mu.Unlock()
		var buf bytes.Buffer
		rel, err := d.Deployer.Deploy(d.ctx, deploy.Request{App: app, Ref: ref, Trigger: trigger, Out: &buf})
		if rel != nil && rel.SHA != "" {
			d.updateApp(app, func(a *state.App) { a.LastSeen = rel.SHA })
		}
		if err != nil {
			slog.Warn("deploy failed", "app", app, "trigger", trigger, "err", err, "output", lastLines(buf.String(), 20))
		}
	}()
	return true
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- scheduled jobs ----

// scheduleLoop runs the nightly Postgres dumps (default 03:00 local, keep
// 14) and refreshes the webhook IP filter daily.
func (d *Daemon) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg := d.cfg()
		now := time.Now()
		today := now.Format("2006-01-02")
		marker := d.Store.Path("backups", ".nightly")
		last, _ := os.ReadFile(marker)
		if now.Hour() >= cfg.PGBackupHour && strings.TrimSpace(string(last)) != today {
			state.WriteFile(marker, []byte(today+"\n"), 0o600)
			d.nightlyBackups(ctx, cfg)
			if _, err := offsite.LoadConfig(d.Store.Root); err == nil {
				go func() {
					if _, err := d.offsiteRun(ctx, false, "nightly", io.Discard); err != nil {
						slog.Warn("off-Mac backup", "err", err)
					}
				}()
			}
			if cfg.CFIPFilter {
				if err := d.refreshIPFilter(ctx); err != nil {
					slog.Warn("webhook ip filter", "err", err)
				}
			}
		}
	}
}

func (d *Daemon) nightlyBackups(ctx context.Context, cfg *state.Config) {
	svs, _ := d.Store.Services("pg")
	for _, sv := range svs {
		path, err := d.Services.Backup(ctx, sv.Kind, sv.Name, cfg.PGBackupKeep)
		if err != nil {
			d.event(state.Event{Kind: "backup.failed", Message: err.Error()})
			continue
		}
		d.event(state.Event{Kind: "backup", Message: fmt.Sprintf("%s:%s → %s", sv.Kind, sv.Name, filepath.Base(path))})
	}
}

func (d *Daemon) refreshIPFilter(ctx context.Context) error {
	cfg := d.cfg()
	tok := d.cfToken()
	if tok == "" || cfg.CFZoneID == "" {
		return fmt.Errorf("webhook IP filter needs a Cloudflare API token with Zone WAF: Edit")
	}
	ranges, err := d.GitHubAPI("").HookRanges(ctx)
	if err != nil {
		return err
	}
	return d.CloudflareAPI(tok).SetWebhookFilter(ctx, cfg.CFZoneID, "hooks."+cfg.Domain, ranges)
}

// ---- notifications ----

func (d *Daemon) notify(ev state.Event) {
	cfg := d.cfg()
	if cfg.NotifyURL == "" {
		return
	}
	if err := SendNotification(cfg.NotifyURL, cfg.Domain, ev); err != nil {
		slog.Warn("notify", "err", err)
	}
}

// SendNotification posts an event. Slack and Discord read "text" and
// "content"; ntfy gets a plain-text body with a title header.
func SendNotification(url, domain string, ev state.Event) error {
	text := fmt.Sprintf("[berth] %s: %s", ev.Kind, ev.Message)
	if ev.App != "" {
		text = fmt.Sprintf("[berth] %s %s: %s", ev.App, ev.Kind, ev.Message)
	}
	var req *http.Request
	var err error
	if strings.Contains(url, "ntfy") {
		req, err = http.NewRequest("POST", url, strings.NewReader(ev.Message))
		if err == nil {
			req.Header.Set("Title", strings.TrimSpace(fmt.Sprintf("berth %s %s", ev.App, ev.Kind)))
			if ev.Kind != "deploy.ok" {
				req.Header.Set("Priority", "high")
			}
		}
	} else {
		body, _ := json.Marshal(map[string]any{
			"text": text, "content": text,
			"kind": ev.Kind, "app": ev.App, "message": ev.Message, "time": ev.Time, "domain": domain, "fields": ev.Fields,
		})
		req, err = http.NewRequest("POST", url, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: %s", resp.Status)
	}
	return nil
}

// ---- mDNS: <app>.<hostname>.local on the LAN ----

// mdnsRegistry keeps one `dns-sd -P` proxy registration per app so
// <app>.<hostname>.local resolves on the LAN (macOS only advertises the
// bare <hostname>.local by itself).
type mdnsRegistry struct {
	mu    sync.Mutex
	procs map[string]*mdnsProc // host -> registration
	ip    string
}

type mdnsProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *mdnsProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func newMDNS() *mdnsRegistry { return &mdnsRegistry{procs: map[string]*mdnsProc{}} }

func lanIP() string {
	c, err := net.Dial("udp", "192.0.2.1:9") // no packets sent; picks the default route's source
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func (m *mdnsRegistry) sync(cfg *state.Config, apps []*state.App) {
	if m == nil || cfg.Hostname == "" || cfg.CaddyHTTPS == "" || os.Getenv("BERTH_NO_MDNS") != "" {
		return
	}
	if _, err := exec.LookPath("dns-sd"); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ip := lanIP()
	if ip != m.ip {
		for h, p := range m.procs {
			p.cmd.Process.Kill()
			delete(m.procs, h)
		}
		m.ip = ip
	}
	if ip == "" {
		return
	}
	want := map[string]string{}
	for _, a := range apps {
		want[a.Name+"."+cfg.Hostname+".local"] = a.Name
	}
	for h, p := range m.procs {
		if _, ok := want[h]; !ok || p.exited() {
			p.cmd.Process.Kill()
			delete(m.procs, h)
		}
	}
	port := "443"
	if i := strings.LastIndex(cfg.CaddyHTTPS, ":"); i >= 0 {
		port = cfg.CaddyHTTPS[i+1:]
	}
	for h, app := range want {
		if _, ok := m.procs[h]; ok {
			continue
		}
		cmd := exec.Command("dns-sd", "-P", app, "_https._tcp", "local", port, h, ip)
		if err := cmd.Start(); err != nil {
			continue
		}
		p := &mdnsProc{cmd: cmd, done: make(chan struct{})}
		go func() { cmd.Wait(); close(p.done) }()
		m.procs[h] = p
	}
}

func (m *mdnsRegistry) stopAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, p := range m.procs {
		p.cmd.Process.Kill()
		delete(m.procs, h)
	}
}
