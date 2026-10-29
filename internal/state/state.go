// Package state owns everything under ~/.berth: config, apps, releases,
// env files and services. All writes are atomic (temp file + rename).
// berthd is the only writer; the CLI reads these files directly for
// offline, read-only commands.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Home returns the Berth root, honouring BERTH_HOME for tests and
// side-by-side installs.
func Home() string {
	if h := os.Getenv("BERTH_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".berth")
}

// Store is a handle on a Berth root directory.
type Store struct{ Root string }

func Open() *Store { return &Store{Root: Home()} }

func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

// SocketPath is ~/.berth/berthd.sock, or a short per-root path in $TMPDIR
// when that would exceed the 104-byte limit macOS puts on unix socket paths.
func (s *Store) SocketPath() string {
	p := s.Path("berthd.sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(s.Root))
	return filepath.Join(os.TempDir(), "berth-"+hex.EncodeToString(sum[:6])+".sock")
}

func (s *Store) AppDir(app string) string { return s.Path("apps", app) }
func (s *Store) SecretsDir() string       { return s.Path("secrets") }
func (s *Store) EventsPath() string       { return s.Path("events.log") }

// Config is ~/.berth/config.json.
type Config struct {
	Domain       string `json:"domain"`
	Hostname     string `json:"hostname"` // short Mac hostname, for <app>.<hostname>.local
	NoTunnel     bool   `json:"no_tunnel,omitempty"`
	Unattended   bool   `json:"unattended,omitempty"`
	CFIPFilter   bool   `json:"cf_ip_filter,omitempty"`
	TunnelID     string `json:"tunnel_id,omitempty"`
	TunnelName   string `json:"tunnel_name,omitempty"`
	TunnelRemote bool   `json:"tunnel_remote,omitempty"` // cloudflared runs with the tunnel token and follows the remote ingress
	CFAccountID  string `json:"cf_account_id,omitempty"`
	CFZoneID     string `json:"cf_zone_id,omitempty"`
	GitHubUser   string `json:"github_user,omitempty"`
	NotifyURL    string `json:"notify_url,omitempty"`
	Network      string `json:"network"`
	Gateway      string `json:"gateway,omitempty"` // berth network gateway; services publish here

	WebhookAddr    string `json:"webhook_addr"`   // 127.0.0.1:7777
	CaddyAdmin     string `json:"caddy_admin"`    // 127.0.0.1:2019
	CaddyHTTP      string `json:"caddy_http"`     // 127.0.0.1:7780 (tunnel side; macOS allows unprivileged <1024 binds only on the wildcard address)
	CaddyHTTPS     string `json:"caddy_https"`    // :443 (LAN), empty disables
	CaddyLANHTTP   string `json:"caddy_lan_http"` // :80 LAN redirect to HTTPS, empty disables
	TunnelMetrics  string `json:"tunnel_metrics"` // cloudflared --metrics
	DNSAddr        string `json:"dns_addr"`       // berthd\'s SRV responder for Caddy upstreams
	AuthAddr       string `json:"auth_addr"`      // berthd's sign-in server, behind Caddy
	AIAddr         string `json:"ai_addr"`        // berthd's AI gateway, behind Caddy
	AdminAddr      string `json:"admin_addr"`     // berthd's admin page, behind Caddy and sign-in
	AIPort         int    `json:"ai_port"`        // where apps reach the AI gateway on the berth network gateway
	PortMin        int    `json:"port_min"`
	PortMax        int    `json:"port_max"`
	ServicePortMin int    `json:"service_port_min"`
	ServicePortMax int    `json:"service_port_max"`

	DefaultMemory   string `json:"default_memory"`
	DefaultCPUs     string `json:"default_cpus"`
	KeepReleases    int    `json:"keep_releases"`
	PGBackupHour    int    `json:"pg_backup_hour"`
	PGBackupKeep    int    `json:"pg_backup_keep"`
	DevIdleHours    int    `json:"dev_idle_hours"` // stop a dev version after this long without a deploy or request
	Launchd         bool   `json:"launchd"`
	AutoHealNetwork *bool  `json:"auto_heal_network,omitempty"` // restart the container system if containers lose outbound network (default on)
	ContainerTested string `json:"container_tested"`
}

func DefaultConfig() *Config {
	return &Config{
		Hostname:        LocalHostName(),
		Network:         "berth",
		WebhookAddr:     "127.0.0.1:7777",
		CaddyAdmin:      "127.0.0.1:2019",
		CaddyHTTP:       "127.0.0.1:7780",
		CaddyLANHTTP:    ":80",
		CaddyHTTPS:      ":443",
		TunnelMetrics:   "127.0.0.1:20241",
		DNSAddr:         "127.0.0.1:7753",
		AuthAddr:        "127.0.0.1:7781",
		AIAddr:          "127.0.0.1:7782",
		AdminAddr:       "127.0.0.1:7783",
		AIPort:          7790,
		PortMin:         8001,
		PortMax:         8999,
		ServicePortMin:  6001,
		ServicePortMax:  6999,
		DefaultMemory:   "512M",
		DefaultCPUs:     "1",
		KeepReleases:    5,
		PGBackupHour:    3,
		PGBackupKeep:    14,
		DevIdleHours:    12,
		ContainerTested: "1.6.1",
	}
}

// LocalHostName is the Mac's Bonjour name (what <name>.local resolves to),
// lowercased. os.Hostname can return a reverse-DNS name instead.
func LocalHostName() string {
	if out, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		if h := strings.TrimSpace(string(out)); h != "" {
			return strings.ToLower(h)
		}
	}
	host, _ := os.Hostname()
	return strings.ToLower(strings.TrimSuffix(strings.Split(host, ".")[0], ".local"))
}

// HealNetwork reports whether berthd may restart the container system when
// containers lose outbound network.
func (c *Config) HealNetwork() bool { return c.AutoHealNetwork == nil || *c.AutoHealNetwork }

var ErrNotInitialized = errors.New("berth is not initialized (run `berth init`)")

func (s *Store) Config() (*Config, error) {
	c := DefaultConfig()
	if err := readJSON(s.Path("config.json"), c); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotInitialized
		}
		return nil, err
	}
	return c, nil
}

func (s *Store) SaveConfig(c *Config) error { return writeJSON(s.Path("config.json"), c, 0o600) }

// Mount is a persistent folder bind-mounted into every release.
type Mount struct {
	Path   string `json:"path"`   // inside the container
	Source string `json:"source"` // on the host
}

// SQLite is an app's own SQLite database (design.md, SQLite): a container
// volume, an ext4 disk image the VM attaches, mounted at SQLiteDir. A
// volume can be attached to one VM at a time, so only one container ever
// has the database open.
type SQLite struct {
	Volume    string    `json:"volume"`
	EnvVar    string    `json:"env_var"` // the URL's variable; SQLITE_PATH is set too
	CreatedAt time.Time `json:"created_at"`
}

const (
	SQLiteDir     = "/data/sqlite"
	SQLiteFile    = "app.db"
	SQLitePath    = SQLiteDir + "/" + SQLiteFile
	SQLitePathVar = "SQLITE_PATH"
	// SQLiteURL is the form Django (dj-database-url), SQLAlchemy and most
	// URL parsers take: four slashes for an absolute path.
	SQLiteURL = "sqlite:///" + SQLitePath
)

// SQLiteVolume names an app's SQLite volume.
func SQLiteVolume(app string) string { return "berth-sqlite-" + app }

// Volumes is every -v an app's containers get: its storage folders and
// its SQLite volume.
func (a *App) Volumes() []string {
	var vols []string
	for _, m := range a.Mounts {
		vols = append(vols, m.Source+":"+m.Path)
	}
	if a.SQLite != nil {
		vols = append(vols, a.SQLite.Volume+":"+SQLiteDir)
	}
	return vols
}

type Webhook struct {
	ID     int64  `json:"id,omitempty"`
	Secret string `json:"secret"`
}

// DevToken lets an agent past the dev version's access check. As, if set,
// is the pretend user it is signed in as.
type DevToken struct {
	Hash    string    `json:"hash"` // sha256 hex
	As      string    `json:"as,omitempty"`
	Created time.Time `json:"created"`
}

// DevName is the dev version of app.
func DevName(app string) string { return app + "-dev" }

// PreviewName is the app for pull request n's preview: <app>-pr-<n>.
func PreviewName(app string, n int) string { return fmt.Sprintf("%s-pr-%d", app, n) }

var previewNameRe = regexp.MustCompile(`-pr-[0-9]+$`)

// IsPreviewName reports whether name has a preview's -pr-<n> suffix.
func IsPreviewName(name string) bool { return previewNameRe.MatchString(name) }

type BasicAuth struct {
	User string `json:"user"`
	Hash string `json:"hash"` // bcrypt
}

// App is apps/<app>/app.json.
type App struct {
	Name      string     `json:"name"`
	Repo      string     `json:"repo"` // owner/name or a git URL/path
	Branch    string     `json:"branch"`
	Path      string     `json:"path,omitempty"` // monorepo subdirectory
	Private   bool       `json:"private,omitempty"`
	NoWebhook bool       `json:"no_webhook,omitempty"`
	Webhook   Webhook    `json:"webhook"`
	Domains   []string   `json:"domains,omitempty"`
	Mounts    []Mount    `json:"mounts,omitempty"`
	SQLite    *SQLite    `json:"sqlite,omitempty"`
	Slots     []int      `json:"slots,omitempty"` // two fixed host ports; releases alternate between them
	Links     []string   `json:"links,omitempty"` // "pg:mydb", "redis:cache"
	Auth      *BasicAuth `json:"auth,omitempty"`
	Access    *Access    `json:"access,omitempty"` // sign-in policy; nil = none
	DevOf     string     `json:"dev_of,omitempty"` // set on <app>-dev: the app it is the dev version of
	DevOff    bool       `json:"dev_off,omitempty"`
	DevTokens []DevToken `json:"dev_tokens,omitempty"` // bearer tokens for the dev version (hashes)
	// Preview is the pull request number on a PR preview (which also has
	// DevOf set). On the parent, PreviewsOn turns them on and PreviewMax
	// caps how many run at once.
	Preview    int    `json:"preview,omitempty"`
	PreviewsOn bool   `json:"previews_on,omitempty"`
	PreviewMax int    `json:"preview_max,omitempty"`
	AI         *AppAI `json:"ai,omitempty"` // access to the AI gateway
	DeployKey  bool   `json:"deploy_key,omitempty"`
	Stopped    bool   `json:"stopped,omitempty"`
	// Scale is how many containers run per worker process type; a type
	// that isn't listed runs one.
	Scale      map[string]int `json:"scale,omitempty"`
	PollETag   string         `json:"poll_etag,omitempty"`
	LastSeen   string         `json:"last_seen,omitempty"`  // last branch head we acted on
	Deliveries []string       `json:"deliveries,omitempty"` // recent webhook delivery ids
	CreatedAt  time.Time      `json:"created_at"`
}

// WorkerCount is how many containers of process type t should run.
func (a *App) WorkerCount(t string) int {
	if n, ok := a.Scale[t]; ok {
		return n
	}
	return 1
}

// IsGitHub reports whether Repo is a GitHub owner/name shorthand.
func (a *App) IsGitHub() bool { return githubRepoRe.MatchString(a.Repo) }

var githubRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// CloneURL is the URL git fetches from.
func (a *App) CloneURL() string {
	if a.IsGitHub() {
		if a.DeployKey {
			return "git@github.com:" + a.Repo + ".git"
		}
		return "https://github.com/" + a.Repo + ".git"
	}
	return a.Repo
}

var appNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var reservedNames = map[string]bool{"hooks": true, "berth": true, "www": true, "auth": true, "admin": true}

// ValidateAppName enforces DNS-label names that do not collide with
// Berth's own hostnames or service container names.
func ValidateAppName(name string) error {
	if !appNameRe.MatchString(name) {
		return fmt.Errorf("invalid app name %q: use lowercase letters, digits and dashes (max 63)", name)
	}
	if reservedNames[name] || strings.HasPrefix(name, "pg-") || strings.HasPrefix(name, "redis-") {
		return fmt.Errorf("app name %q is reserved", name)
	}
	return nil
}

func (s *Store) AppExists(name string) bool {
	_, err := os.Stat(s.Path("apps", name, "app.json"))
	return err == nil
}

func (s *Store) App(name string) (*App, error) {
	a := &App{}
	if err := readJSON(s.Path("apps", name, "app.json"), a); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no app named %q (see `berth status`)", name)
		}
		return nil, err
	}
	a.Name = name // the directory is authoritative, not the file's contents
	return a, nil
}

func (s *Store) SaveApp(a *App) error {
	if err := os.MkdirAll(s.AppDir(a.Name), 0o700); err != nil {
		return err
	}
	return writeJSON(s.Path("apps", a.Name, "app.json"), a, 0o600)
}

func (s *Store) Apps() ([]*App, error) {
	entries, err := os.ReadDir(s.Path("apps"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var apps []*App
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		a, err := s.App(e.Name())
		if err != nil {
			continue
		}
		apps = append(apps, a)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].CreatedAt.Before(apps[j].CreatedAt) })
	return apps, nil
}

// Release statuses.
const (
	StatusBuilding  = "building"
	StatusReleasing = "releasing" // running the release command
	StatusStarting  = "starting"
	StatusRunning   = "running"
	StatusFailed    = "failed"
	StatusStopped   = "stopped" // superseded or stopped by the user
	StatusCrashed   = "crashed"
)

// Release is apps/<app>/releases/<n>.json.
type Release struct {
	N          int               `json:"n"`
	SHA        string            `json:"sha"`
	Image      string            `json:"image"`
	Env        map[string]string `json:"env"`
	HostPort   int               `json:"host_port"`
	Container  string            `json:"container,omitempty"` // live container, "" when not running
	Pruned     bool              `json:"pruned,omitempty"`    // image garbage-collected
	Status     string            `json:"status"`
	Trigger    string            `json:"trigger,omitempty"`
	Error      string            `json:"error,omitempty"`
	Message    string            `json:"message,omitempty"` // commit subject
	CreatedAt  time.Time         `json:"created_at"`
	StartedAt  time.Time         `json:"started_at,omitempty"`
	BuildSecs  float64           `json:"build_secs,omitempty"`
	RollbackOf int               `json:"rollback_of,omitempty"`
	// FailedStage is build, release or start when Status is failed.
	FailedStage string `json:"failed_stage,omitempty"`
	// ReleaseCmd is the release command that ran before the swap
	// (BERTH_RELEASE_COMMAND, else the Procfile's release: line, kept in
	// ProcfileRelease so config-only releases can run it again).
	ReleaseCmd      string  `json:"release_cmd,omitempty"`
	ProcfileRelease string  `json:"procfile_release,omitempty"`
	ReleaseSecs     float64 `json:"release_secs,omitempty"`
	// Backups are the Postgres dumps taken just before the release command
	// ran ("pg:name" → file), for `berth rollback --with-data`.
	Backups map[string]string `json:"backups,omitempty"`
	DataOf  int               `json:"data_of,omitempty"` // the release that was current when they were taken
	// Processes are the Procfile's worker types (not web or release) →
	// command, and Workers their running containers.
	Processes map[string]string `json:"processes,omitempty"`
	Workers   []Worker          `json:"workers,omitempty"`
	// Cron is app.json's "cron" list when this release was built.
	Cron []CronJob `json:"cron,omitempty"`
}

// Worker is one running container of a worker process type.
type Worker struct {
	Type      string `json:"type"`
	Index     int    `json:"index"` // 1-based
	Container string `json:"container"`
}

// Name is how logs and status show it: worker.1.
func (w Worker) Name() string { return fmt.Sprintf("%s.%d", w.Type, w.Index) }

// CronJob is a scheduled command (app.json, Dokku's format).
type CronJob struct {
	Command  string `json:"command"`
	Schedule string `json:"schedule"`
}

// ContainerName is the per-release container name.
func ContainerName(app string, n int) string { return fmt.Sprintf("berth-%s-%d", app, n) }

func ImageName(app string, n int) string { return fmt.Sprintf("berth/%s:%d", app, n) }

func (s *Store) Release(app string, n int) (*Release, error) {
	r := &Release{}
	if err := readJSON(s.Path("apps", app, "releases", strconv.Itoa(n)+".json"), r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) SaveRelease(app string, r *Release) error {
	return writeJSON(s.Path("apps", app, "releases", strconv.Itoa(r.N)+".json"), r, 0o600)
}

// Releases returns all releases, newest first.
func (s *Store) Releases(app string) ([]*Release, error) {
	entries, err := os.ReadDir(s.Path("apps", app, "releases"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rs []*Release
	for _, e := range entries {
		n, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if r, err := s.Release(app, n); err == nil {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].N > rs[j].N })
	return rs, nil
}

func (s *Store) NextReleaseNumber(app string) int {
	rs, _ := s.Releases(app)
	if len(rs) == 0 {
		return 1
	}
	return rs[0].N + 1
}

// Current returns the current release number, 0 if none.
func (s *Store) Current(app string) int {
	b, err := os.ReadFile(s.Path("apps", app, "current"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func (s *Store) SetCurrent(app string, n int) error {
	return atomicWrite(s.Path("apps", app, "current"), []byte(strconv.Itoa(n)+"\n"), 0o644)
}

func (s *Store) CurrentRelease(app string) *Release {
	n := s.Current(app)
	if n == 0 {
		return nil
	}
	r, err := s.Release(app, n)
	if err != nil {
		return nil
	}
	return r
}

// Service is services/<kind>/<name>/service.json.
type Service struct {
	Kind       string          `json:"kind"` // pg | redis
	Name       string          `json:"name"`
	Image      string          `json:"image"`
	Password   string          `json:"password"`
	Port       int             `json:"port"`                  // published on the berth gateway
	ExposePort int             `json:"expose_port,omitempty"` // published on 127.0.0.1
	Memory     string          `json:"memory"`
	Volume     string          `json:"volume"`
	Links      map[string]Link `json:"links,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Link struct {
	Role     string `json:"role"`
	Password string `json:"password"`
	EnvVar   string `json:"env_var"`
	DB       string `json:"db,omitempty"` // Postgres database, if not the service's own (dev versions)
}

func ServiceContainer(kind, name string) string { return kind + "-" + name }

func (s *Store) Service(kind, name string) (*Service, error) {
	sv := &Service{}
	if err := readJSON(s.Path("services", kind, name, "service.json"), sv); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s service named %q", kind, name)
		}
		return nil, err
	}
	if sv.Links == nil {
		sv.Links = map[string]Link{}
	}
	return sv, nil
}

func (s *Store) SaveService(sv *Service) error {
	return writeJSON(s.Path("services", sv.Kind, sv.Name, "service.json"), sv, 0o600)
}

func (s *Store) DeleteService(kind, name string) error {
	return os.RemoveAll(s.Path("services", kind, name))
}

func (s *Store) Services(kind string) ([]*Service, error) {
	var kinds []string
	if kind != "" {
		kinds = []string{kind}
	} else {
		kinds = []string{"pg", "redis"}
	}
	var out []*Service
	for _, k := range kinds {
		entries, err := os.ReadDir(s.Path("services", k))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if sv, err := s.Service(k, e.Name()); err == nil {
				out = append(out, sv)
			}
		}
	}
	return out, nil
}

// ---- file helpers ----

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), mode)
}

// WriteFile atomically writes data to path, creating parent directories.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	return atomicWrite(path, data, mode)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AccessLog is where Caddy writes the app's request log. The name is unique
// to this incarnation of the app: Caddy keeps a log file open across
// reloads for as long as some logger names it, so an app destroyed and
// created again under the same name would otherwise go on writing into the
// deleted file.
func (s *Store) AccessLog(a *App) string {
	return s.Path("apps", a.Name, "logs", "access."+strconv.FormatInt(a.CreatedAt.Unix(), 36)+".log")
}

// AccessLogs lists every request log of an app, oldest first: current and
// rotated files, including ones from before AccessLog's naming.
func (s *Store) AccessLogs(app string) []string {
	files, _ := filepath.Glob(s.Path("apps", app, "logs", "access*.log"))
	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(files[i])
		fj, _ := os.Stat(files[j])
		if fi == nil || fj == nil {
			return files[i] < files[j]
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	return files
}
