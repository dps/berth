package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/admin"
	"github.com/dps/berth/internal/analytics"
	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
)

// ---- admin page (design.md, Admin page) ----

// adminHost is where the admin page is served, or "" when it is off or
// sign-in isn't set up (the page is for @owners only, so it needs sign-in).
func (d *Daemon) adminHost(cfg *state.Config) string {
	ac, err := d.Store.AuthConfig()
	if err != nil || ac.AdminHost == "" || d.signInHost(cfg) == "" {
		return ""
	}
	return ac.AdminHost
}

// startAnalytics tails every app's access log into d.stats.
func (d *Daemon) startAnalytics(ctx context.Context) {
	d.stats = analytics.NewStats()
	t := analytics.NewTailer(d.stats, func() map[string][]string {
		apps, _ := d.Store.Apps()
		files := map[string][]string{}
		for _, a := range apps {
			files[a.Name] = d.Store.AccessLogs(a.Name)
		}
		return files
	})
	go t.Run(5*time.Second, ctx.Done())
}

func (d *Daemon) startAdmin(cfg *state.Config) (*http.Server, error) {
	ln, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		return nil, fmt.Errorf("admin listener %s: %w", cfg.AdminAddr, err)
	}
	srv := &http.Server{Handler: d.adminHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go srv.Serve(ln)
	return srv, nil
}

type viewerKey struct{}

// statusCache serves the admin page a recent Status without waiting for a
// fresh one (collecting container stats takes seconds): a result up to
// statusMaxAge old is returned as is, an older one is returned and
// refreshed in the background, and only the first request waits.
type statusCache struct {
	mu         sync.Mutex
	st         *Status
	at         time.Time
	refreshing bool
}

const statusMaxAge = 4 * time.Second

func (d *Daemon) adminStatus(ctx context.Context) (*Status, error) {
	c := &d.statusCache
	c.mu.Lock()
	st, age := c.st, time.Since(c.at)
	if st != nil && age > statusMaxAge && !c.refreshing {
		c.refreshing = true
		go func() {
			fresh, err := d.Status(context.Background())
			c.mu.Lock()
			if err == nil {
				c.st, c.at = fresh, time.Now()
			}
			c.refreshing = false
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	if st != nil {
		return st, nil
	}
	fresh, err := d.Status(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.st, c.at = fresh, time.Now()
	c.mu.Unlock()
	return fresh, nil
}

// adminHandler serves the page and its JSON API. Caddy only sends requests
// that sign-in let through (allow @owners), and strips X-Berth-* headers
// from clients; the handler checks berthd's signed identity anyway, so a
// request that reached the port some other way gets nothing.
func (d *Daemon) adminHandler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(admin.UI, "ui")
	static := http.FileServerFS(ui)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path, ".") {
			r.URL.Path = "/" // client-side routes
		}
		// Small files, owners only: never cached, so an upgraded Berth's page
		// is what the next load gets.
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/overview", d.apiOverview)
	mux.HandleFunc("GET /api/apps/{app}", d.apiApp)
	mux.HandleFunc("GET /api/apps/{app}/live", d.apiLive)
	mux.HandleFunc("GET /api/apps/{app}/logs", d.apiLogs)
	mux.HandleFunc("GET /api/events", d.apiEvents)
	mux.HandleFunc("GET /api/load", d.apiLoad)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		host := d.adminHost(d.cfg())
		if host == "" || d.authKey == nil {
			http.Error(w, "the admin page is off (berth admin:enable)", http.StatusNotFound)
			return
		}
		claims, err := auth.VerifyIdentity(d.authKey.Public().(ed25519.PublicKey), r.Header.Get("X-Berth-Identity"), host, time.Now())
		if err != nil {
			slog.Warn("admin: refused", "err", err, "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, claims)))
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// AppSummary is an app's last 24 hours, for the overview.
type AppSummary struct {
	Totals   analytics.Totals `json:"totals"`
	Previous analytics.Totals `json:"previous"`
	Spark    []int            `json:"spark"` // requests per hour, oldest first
	Errors   []int            `json:"errors"`
}

type Overview struct {
	Viewer    *auth.IdentityClaims  `json:"viewer"`
	Host      string                `json:"host"`
	Status    *Status               `json:"status"`
	Summaries map[string]AppSummary `json:"summaries"`
	All       *analytics.Report     `json:"all"` // every app as one, hourly, for the last 24 hours
	Events    []state.Event         `json:"events"`
	AI        *AIStatus             `json:"ai,omitempty"`
	Now       time.Time             `json:"now"`
}

func (d *Daemon) apiOverview(w http.ResponseWriter, r *http.Request) {
	st, err := d.adminStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ov := &Overview{Viewer: r.Context().Value(viewerKey{}).(*auth.IdentityClaims), Host: d.adminHost(d.cfg()), Status: st, Summaries: map[string]AppSummary{}, Now: time.Now()}
	var names []string
	for _, a := range st.Apps {
		names = append(names, a.Name)
		rep := d.stats.Query(a.Name, 24*time.Hour, time.Hour, 0, false)
		s := AppSummary{Totals: rep.Totals, Previous: rep.Previous}
		for _, p := range rep.Series {
			s.Spark = append(s.Spark, p.Req)
			s.Errors = append(s.Errors, p.S5)
		}
		ov.Summaries[a.Name] = s
	}
	ov.All = d.stats.QueryAll(names, 24*time.Hour, time.Hour)
	ov.Events = d.recentEvents("", 40)
	if d.Store.AIConfigured() {
		ov.AI, _ = d.aiStatus(r.Context())
	}
	writeJSON(w, ov)
}

// ReleaseInfo is a release without its env snapshot (which holds secrets).
type ReleaseInfo struct {
	N           int       `json:"n"`
	SHA         string    `json:"sha"`
	Message     string    `json:"message,omitempty"`
	Status      string    `json:"status"`
	Trigger     string    `json:"trigger,omitempty"`
	Error       string    `json:"error,omitempty"`
	FailedStage string    `json:"failed_stage,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	BuildSecs   float64   `json:"build_secs,omitempty"`
	ReleaseSecs float64   `json:"release_secs,omitempty"`
	ReleaseCmd  string    `json:"release_cmd,omitempty"`
	Current     bool      `json:"current,omitempty"`
}

// AppInfo is how an app is set up (no env values).
type AppInfo struct {
	Repo      string        `json:"repo,omitempty"`
	Branch    string        `json:"branch,omitempty"`
	Path      string        `json:"path,omitempty"`
	Private   bool          `json:"private,omitempty"`
	Domains   []string      `json:"domains,omitempty"`
	Links     []string      `json:"links,omitempty"`
	Mounts    []string      `json:"mounts,omitempty"`
	SQLite    *SQLiteInfo   `json:"sqlite,omitempty"`
	Access    *state.Access `json:"access,omitempty"`
	BasicAuth bool          `json:"basic_auth,omitempty"`
	DevOf     string        `json:"dev_of,omitempty"`
	Dev       string        `json:"dev,omitempty"` // its dev version, if it has one
	AI        *state.AppAI  `json:"ai,omitempty"`
	EnvKeys   []string      `json:"env_keys"` // names only
	Created   time.Time     `json:"created"`
}

type AppDetail struct {
	App      AppStatus         `json:"app"`
	Info     AppInfo           `json:"info"`
	Report   *analytics.Report `json:"report"`
	Releases []ReleaseInfo     `json:"releases"`
	Events   []state.Event     `json:"events"`
	AIUsage  any               `json:"ai_usage,omitempty"`
}

var ranges = map[string][2]time.Duration{
	"1h":  {time.Hour, time.Minute},
	"24h": {24 * time.Hour, 15 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 2 * time.Hour},
}

func (d *Daemon) apiApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	a, err := d.Store.App(name)
	if err != nil {
		http.Error(w, "no such app", http.StatusNotFound)
		return
	}
	rg, ok := ranges[r.URL.Query().Get("range")]
	if !ok {
		rg = ranges["24h"]
	}
	st, err := d.adminStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	det := &AppDetail{Report: d.stats.Query(name, rg[0], rg[1], 10, true), Releases: []ReleaseInfo{}}
	for _, as := range st.Apps {
		if as.Name == name {
			det.App = as
		}
	}
	env, _ := d.Store.Env(name)
	info := AppInfo{Repo: a.Repo, Branch: a.Branch, Path: a.Path, Private: a.Private, Domains: a.Domains, Links: a.Links,
		Access: a.Access, BasicAuth: a.Auth != nil, DevOf: a.DevOf, AI: a.AI, Created: a.CreatedAt, EnvKeys: []string{}}
	for _, m := range a.Mounts {
		info.Mounts = append(info.Mounts, m.Path)
	}
	if a.SQLite != nil {
		info.SQLite = &SQLiteInfo{App: a.Name, Volume: a.SQLite.Volume, EnvVar: a.SQLite.EnvVar, DevOf: a.DevOf, LastBackup: d.latestSQLiteBackup(a.Name)}
	}
	for k := range env {
		if !strings.HasPrefix(k, "BERTH_") {
			info.EnvKeys = append(info.EnvKeys, k)
		}
	}
	slices.Sort(info.EnvKeys)
	if dev, err := d.Store.App(state.DevName(name)); err == nil && dev.DevOf == name {
		info.Dev = dev.Name
	}
	if info.AI != nil {
		ai := *info.AI
		ai.TokenHash = ""
		info.AI = &ai
		if d.ai != nil {
			det.AIUsage = d.ai.Usage.Summary(time.Now().Format("2006-01"), name)
		}
	}
	det.Info = info
	cur := d.Store.Current(name)
	rels, _ := d.Store.Releases(name)
	for _, rel := range rels {
		if len(det.Releases) == 15 {
			break
		}
		det.Releases = append(det.Releases, ReleaseInfo{N: rel.N, SHA: rel.SHA, Message: rel.Message, Status: rel.Status, Trigger: rel.Trigger,
			Error: rel.Error, FailedStage: rel.FailedStage, CreatedAt: rel.CreatedAt, StartedAt: rel.StartedAt, BuildSecs: rel.BuildSecs,
			ReleaseSecs: rel.ReleaseSecs, ReleaseCmd: rel.ReleaseCmd, Current: rel.N == cur})
	}
	det.Events = d.recentEvents(name, 30)
	writeJSON(w, det)
}

func (d *Daemon) apiLive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if !d.Store.AppExists(name) {
		http.Error(w, "no such app", http.StatusNotFound)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	reqs, seq := d.stats.RecentSince(name, since)
	writeJSON(w, map[string]any{"requests": reqs, "seq": seq})
}

var loadRanges = map[string][2]time.Duration{
	"1h":  {time.Hour, time.Minute},
	"24h": {24 * time.Hour, 10 * time.Minute},
}

func (d *Daemon) apiLoad(w http.ResponseWriter, r *http.Request) {
	rg, ok := loadRanges[r.URL.Query().Get("range")]
	if !ok {
		rg = loadRanges["1h"]
	}
	rep := d.load.query(time.Now(), rg[0], rg[1])
	writeJSON(w, rep)
}

func (d *Daemon) apiLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if !d.Store.AppExists(name) {
		http.Error(w, "no such app", http.StatusNotFound)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 2000 {
		n = 300
	}
	lines, _ := state.TailLines(d.Store.Path("apps", name, "logs", "app.log"), n)
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, map[string]any{"lines": lines})
}

func (d *Daemon) apiEvents(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	writeJSON(w, map[string]any{"events": d.recentEvents(r.URL.Query().Get("app"), n)})
}

// recentEvents returns up to n events, newest first, optionally for one
// app (and its dev version).
func (d *Daemon) recentEvents(app string, n int) []state.Event {
	lines, _ := state.TailLines(d.Store.Path("events.log"), 3000)
	out := []state.Event{}
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		var ev state.Event
		if json.Unmarshal([]byte(lines[i]), &ev) != nil {
			continue
		}
		if app != "" && ev.App != app {
			continue
		}
		out = append(out, ev)
	}
	return out
}

type AdminParams struct {
	Host    string `json:"host,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Disable bool   `json:"disable,omitempty"`
}

// hAdminEnable turns the admin page on (or off), at admin.<domain> unless
// --host says otherwise.
func (d *Daemon) hAdminEnable(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AdminParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg := d.cfg()
	ac, err := d.Store.AuthConfig()
	if err != nil {
		return nil, err
	}
	if p.Disable {
		ac.AdminHost = ""
		if err := d.Store.SaveAuthConfig(ac); err != nil {
			return nil, err
		}
		fmt.Fprintln(out, "-----> Admin page off")
		return map[string]string{}, d.Apply(ctx)
	}
	if d.signInHost(cfg) == "" {
		return nil, fmt.Errorf("the admin page is for the @owners group, so it needs sign-in first (berth auth:setup google)")
	}
	if p.Host == "" {
		p.Host = "admin." + cfg.Domain
	}
	p.Host = strings.ToLower(p.Host)
	if p.Host != ac.AdminHost {
		if err := d.checkBerthHost(ctx, cfg, p.Host, "admin"); err != nil {
			return nil, err
		}
	}
	if p.Owner != "" {
		if err := state.ValidateAllowEntry(p.Owner); err != nil || strings.HasPrefix(p.Owner, "@") {
			return nil, fmt.Errorf("--owner takes an email address")
		}
		if !slices.Contains(ac.Groups[devOwnersGroup], p.Owner) {
			ac.Groups[devOwnersGroup] = append(ac.Groups[devOwnersGroup], p.Owner)
			fmt.Fprintf(out, "-----> Added %s to @%s\n", p.Owner, devOwnersGroup)
		}
	}
	ac.AdminHost = p.Host
	if err := d.Store.SaveAuthConfig(ac); err != nil {
		return nil, err
	}
	d.configureAuth()
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	url := "https://" + p.Host
	fmt.Fprintf(out, "-----> Admin page at %s, for @%s: %s\n", url, devOwnersGroup, strings.Join(ac.Groups[devOwnersGroup], ", "))
	if len(ac.Groups[devOwnersGroup]) == 0 {
		fmt.Fprintf(out, "       Nobody is in @%s yet: berth auth:group %s you@gmail.com\n", devOwnersGroup, devOwnersGroup)
	}
	return map[string]string{"url": url}, nil
}
