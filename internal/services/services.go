// Package services runs Postgres and Redis as containers on the berth
// network. Each service's data lives in a `container` named volume; Postgres
// is backed up with pg_dump into ~/.berth/backups, which is what Time
// Machine and `berth backup` rely on.
//
// Apple's container networking has no name resolution between containers
// unless an admin creates a DNS domain, so services publish their port on
// the berth network's gateway address (reachable from every container on
// that network, not from the LAN) and links point at gateway:port.
package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

type Kind struct {
	Name          string // pg, redis
	Image         string // default image
	ContainerPort int
	DataDir       string // mount point of the volume
	EnvVar        string // default link variable
	Memory        string
}

var Kinds = map[string]Kind{
	"pg":    {Name: "pg", Image: "postgres:17", ContainerPort: 5432, DataDir: "/var/lib/postgresql/data", EnvVar: "DATABASE_URL", Memory: "512M"},
	"redis": {Name: "redis", Image: "redis:7", ContainerPort: 6379, DataDir: "/data", EnvVar: "REDIS_URL", Memory: "256M"},
}

type Manager struct {
	Store *state.Store
	RT    runtime.Runtime

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock serialises operations that (re)create a service's container, so the
// supervisor never "restarts" a service that is still being created.
func (m *Manager) lock(kind, name string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l, ok := m.locks[kind+":"+name]
	if !ok {
		l = &sync.Mutex{}
		m.locks[kind+":"+name] = l
	}
	return l
}

func Password() string {
	b := make([]byte, 18)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func validName(name string) error {
	if err := state.ValidateAppName(name); err != nil && !strings.Contains(err.Error(), "reserved") {
		return fmt.Errorf("invalid service name %q: use lowercase letters, digits and dashes", name)
	}
	return nil
}

// roleName maps an app name to a Postgres identifier.
func roleName(app string) string { return strings.ReplaceAll(app, "-", "_") }

func dbName(name string) string { return strings.ReplaceAll(name, "-", "_") }

func (m *Manager) envFile(sv *state.Service) string {
	return m.Store.Path("services", sv.Kind, sv.Name, "env")
}

// Create starts a new service container with a fresh volume.
func (m *Manager) Create(ctx context.Context, kind, name, image, memory string, out io.Writer) (*state.Service, error) {
	k, ok := Kinds[kind]
	if !ok {
		return nil, fmt.Errorf("unknown service kind %q", kind)
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	l := m.lock(kind, name)
	l.Lock()
	defer l.Unlock()
	if _, err := m.Store.Service(kind, name); err == nil {
		return nil, fmt.Errorf("%s:%s already exists", kind, name)
	}
	cfg, err := m.Store.Config()
	if err != nil {
		return nil, err
	}
	if image == "" {
		image = k.Image
	}
	if memory == "" {
		memory = k.Memory
	}
	port, err := m.allocatePort(cfg)
	if err != nil {
		return nil, err
	}
	sv := &state.Service{
		Kind: kind, Name: name, Image: image, Password: Password(), Port: port, Memory: memory,
		Volume: "berth-" + kind + "-" + name, Links: map[string]state.Link{}, CreatedAt: time.Now(),
	}
	fmt.Fprintf(out, "-----> Creating %s:%s (%s, volume %s)\n", kind, name, image, sv.Volume)
	if err := m.RT.VolumeCreate(ctx, sv.Volume); err != nil {
		return nil, err
	}
	if err := m.Store.SaveService(sv); err != nil {
		return nil, err
	}
	if err := m.run(ctx, sv, cfg); err != nil {
		return sv, err
	}
	fmt.Fprintf(out, "-----> Waiting for %s:%s to accept connections\n", kind, name)
	if err := m.WaitReady(ctx, sv, 90*time.Second); err != nil {
		return sv, err
	}
	return sv, nil
}

func (m *Manager) allocatePort(cfg *state.Config) (int, error) {
	used := map[int]bool{}
	svs, _ := m.Store.Services("")
	for _, s := range svs {
		used[s.Port] = true
		used[s.ExposePort] = true
	}
	for p := cfg.ServicePortMin; p <= cfg.ServicePortMax; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free service port in %d-%d", cfg.ServicePortMin, cfg.ServicePortMax)
}

// run (re)creates the service container from its state.
func (m *Manager) run(ctx context.Context, sv *state.Service, cfg *state.Config) error {
	k := Kinds[sv.Kind]
	name := state.ServiceContainer(sv.Kind, sv.Name)
	env := map[string]string{}
	var args []string
	switch sv.Kind {
	case "pg":
		env["POSTGRES_PASSWORD"] = sv.Password
		env["POSTGRES_DB"] = dbName(sv.Name)
		env["PGDATA"] = k.DataDir + "/pgdata" // the volume root holds lost+found
	case "redis":
		env["REDIS_PASSWORD"] = sv.Password
		// Users live in an ACL file on the volume: "default" (Berth's own
		// admin login) plus one user per linked app, so unlink can revoke.
		args = []string{"sh", "-c", redisEntrypoint}
	}
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, env[key])
	}
	if err := state.WriteFile(m.envFile(sv), []byte(b.String()), 0o600); err != nil {
		return err
	}
	publish := []string{fmt.Sprintf("%s:%d:%d", cfg.Gateway, sv.Port, k.ContainerPort)}
	if sv.ExposePort != 0 {
		publish = append(publish, fmt.Sprintf("127.0.0.1:%d:%d", sv.ExposePort, k.ContainerPort))
	}
	m.RT.Remove(ctx, name)
	opts := runtime.RunOpts{
		Name:    name,
		Image:   sv.Image,
		Network: cfg.Network,
		Publish: publish,
		EnvFile: m.envFile(sv),
		Memory:  sv.Memory,
		Volumes: []string{sv.Volume + ":" + k.DataDir},
		Labels:  map[string]string{"dev.berth.service": sv.Kind + ":" + sv.Name},
		Detach:  true,
	}
	if len(args) > 0 {
		opts.Entrypoint = args[0]
		opts.Args = args[1:]
	}
	return m.RT.Run(ctx, opts)
}

const redisEntrypoint = `[ -f /data/users.acl ] || printf 'user default on >%s ~* &* +@all\n' "$REDIS_PASSWORD" > /data/users.acl
exec redis-server --aclfile /data/users.acl --appendonly yes --dir /data`

// redisCLI runs redis-cli as Berth's admin user with commands on stdin, so
// app passwords never appear in a command line.
func (m *Manager) redisCLI(ctx context.Context, sv *state.Service, commands string) error {
	var out bytes.Buffer
	err := m.RT.Exec(ctx, state.ServiceContainer("redis", sv.Name),
		[]string{"sh", "-c", `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning`}, true, false, strings.NewReader(commands), &out, nil)
	if err == nil && strings.Contains(out.String(), "ERR") {
		err = errors.New(strings.TrimSpace(out.String()))
	}
	if err != nil {
		return fmt.Errorf("redis-cli: %v", err)
	}
	return nil
}

// ensureRedisACL recreates a Redis container started before per-app users
// existed (it ran with --requirepass and no ACL file). Data persists in the
// volume; Redis is down for about a second.
func (m *Manager) ensureRedisACL(ctx context.Context, sv *state.Service) error {
	name := state.ServiceContainer("redis", sv.Name)
	if m.RT.Exec(ctx, name, []string{"test", "-f", "/data/users.acl"}, false, false, nil, io.Discard, nil) == nil {
		return nil
	}
	cfg, err := m.Store.Config()
	if err != nil {
		return err
	}
	if err := m.run(ctx, sv, cfg); err != nil {
		return err
	}
	return m.WaitReady(ctx, sv, 60*time.Second)
}

func (m *Manager) applyRedisUser(ctx context.Context, sv *state.Service, l state.Link) error {
	if l.Role == "" {
		return nil // a link made before per-app users: uses the default login
	}
	return m.redisCLI(ctx, sv, fmt.Sprintf("ACL SETUSER %s reset on >%s ~* &* +@all\nACL SAVE\n", l.Role, l.Password))
}

// WaitReady polls until the service accepts TCP connections inside its
// container (Postgres's first-boot init uses a socket-only server, so TCP
// readiness means the real server is up).
func (m *Manager) WaitReady(ctx context.Context, sv *state.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	name := state.ServiceContainer(sv.Kind, sv.Name)
	var lastErr error
	for time.Now().Before(deadline) {
		var probe []string
		switch sv.Kind {
		case "pg":
			probe = []string{"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", dbName(sv.Name), "-tAc", "select 1"}
		case "redis":
			probe = []string{"sh", "-c", `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning ping | grep -q PONG`}
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		lastErr = m.RT.Exec(c, name, probe, false, false, nil, io.Discard, nil)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s:%s not ready after %s: %v", sv.Kind, sv.Name, timeout, lastErr)
}

// EnsureRunning starts the service container if it is missing or stopped.
func (m *Manager) EnsureRunning(ctx context.Context, sv *state.Service, cs []runtime.Container) (bool, error) {
	l := m.lock(sv.Kind, sv.Name)
	if !l.TryLock() {
		return false, nil // being created, exposed, upgraded or restored
	}
	defer l.Unlock()
	// Re-read: the service may have changed or gone while we waited.
	sv, err := m.Store.Service(sv.Kind, sv.Name)
	if err != nil {
		return false, nil
	}
	name := state.ServiceContainer(sv.Kind, sv.Name)
	if c, ok := runtime.Find(cs, name); ok && c.Running() {
		return false, nil
	}
	cfg, err := m.Store.Config()
	if err != nil {
		return false, err
	}
	if err := m.run(ctx, sv, cfg); err != nil {
		return true, err
	}
	return true, m.WaitReady(ctx, sv, 90*time.Second)
}

// URL is the link URL an app sees.
func URL(sv *state.Service, l state.Link, gateway string) string {
	switch sv.Kind {
	case "pg":
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s", l.Role, l.Password, gateway, sv.Port, dbName(sv.Name))
	case "redis":
		if l.Role == "" {
			return fmt.Sprintf("redis://:%s@%s:%d", sv.Password, gateway, sv.Port)
		}
		return fmt.Sprintf("redis://%s:%s@%s:%d", l.Role, l.Password, gateway, sv.Port)
	}
	return ""
}

func (m *Manager) psql(ctx context.Context, sv *state.Service, db, sql string) error {
	var out bytes.Buffer
	// SQL (which can contain role passwords) goes over stdin, not argv.
	err := m.RT.Exec(ctx, state.ServiceContainer("pg", sv.Name),
		[]string{"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", db, "-q", "-f", "-"}, true, false, strings.NewReader(sql), &out, nil)
	if err != nil {
		return fmt.Errorf("psql: %v %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLit(s string) string   { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// grantSQL creates or refreshes the app's role. Every linked role is a
// member of <db>_rw, which owns the public schema, and sessions SET ROLE to
// it so objects created by any linked app are usable by all of them.
func grantSQL(sv *state.Service, l state.Link) (cluster string, inDB string) {
	db := dbName(sv.Name)
	group := db + "_rw"
	cluster = fmt.Sprintf(`DO $$ BEGIN
IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %[1]s) THEN CREATE ROLE %[2]s NOLOGIN; END IF;
IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %[3]s) THEN CREATE ROLE %[4]s LOGIN PASSWORD %[5]s;
ELSE ALTER ROLE %[4]s LOGIN PASSWORD %[5]s; END IF;
END $$;
GRANT %[2]s TO %[4]s;
GRANT ALL ON DATABASE %[6]s TO %[2]s;
ALTER ROLE %[4]s IN DATABASE %[6]s SET role = %[1]s;`,
		quoteLit(group), quoteIdent(group), quoteLit(l.Role), quoteIdent(l.Role), quoteLit(l.Password), quoteIdent(db))
	inDB = fmt.Sprintf(`ALTER SCHEMA public OWNER TO %[1]s; GRANT ALL ON SCHEMA public TO %[1]s;`, quoteIdent(group))
	return
}

// Link creates the app's credentials and returns the env var and value to
// set. The caller writes the env and redeploys.
func (m *Manager) Link(ctx context.Context, kind, name, app, envVar string, appEnv state.Env) (string, string, error) {
	sv, err := m.Store.Service(kind, name)
	if err != nil {
		return "", "", err
	}
	cfg, err := m.Store.Config()
	if err != nil {
		return "", "", err
	}
	if envVar != "" {
		if err := state.ValidateEnvKey(envVar); err != nil {
			return "", "", err
		}
	}
	if envVar == "" {
		envVar = Kinds[kind].EnvVar
		// Another service already owns DATABASE_URL: name this one.
		if cur, ok := appEnv[envVar]; ok && !m.ownsValue(sv, cur) {
			envVar = strings.ToUpper(strings.ReplaceAll(kind+"_"+name, "-", "_")) + "_URL"
		}
	}
	l, ok := sv.Links[app]
	if !ok || l.Role == "" {
		// New link, or a Redis link from before per-app users: give the app
		// its own login.
		l = state.Link{Role: roleName(app), Password: Password()}
	}
	l.EnvVar = envVar
	switch kind {
	case "pg":
		if err := m.applyGrants(ctx, sv, l); err != nil {
			return "", "", err
		}
	case "redis":
		if err := m.ensureRedisACL(ctx, sv); err != nil {
			return "", "", err
		}
		if err := m.applyRedisUser(ctx, sv, l); err != nil {
			return "", "", err
		}
	}
	sv.Links[app] = l
	if err := m.Store.SaveService(sv); err != nil {
		return "", "", err
	}
	return envVar, URL(sv, l, cfg.Gateway), nil
}

func (m *Manager) ownsValue(sv *state.Service, v string) bool {
	return strings.Contains(v, fmt.Sprintf(":%d", sv.Port))
}

func (m *Manager) applyGrants(ctx context.Context, sv *state.Service, l state.Link) error {
	cluster, inDB := grantSQL(sv, l)
	if err := m.psql(ctx, sv, "postgres", cluster); err != nil {
		return err
	}
	return m.psql(ctx, sv, dbName(sv.Name), inDB)
}

// Unlink revokes the app's login and returns the env var to remove.
func (m *Manager) Unlink(ctx context.Context, kind, name, app string) (string, error) {
	sv, err := m.Store.Service(kind, name)
	if err != nil {
		return "", err
	}
	l, ok := sv.Links[app]
	if !ok {
		return "", fmt.Errorf("%s is not linked to %s:%s", app, kind, name)
	}
	switch {
	case kind == "pg":
		if err := m.psql(ctx, sv, "postgres", fmt.Sprintf("ALTER ROLE %s NOLOGIN;", quoteIdent(l.Role))); err != nil {
			return "", err
		}
	case kind == "redis" && l.Role != "":
		if err := m.redisCLI(ctx, sv, fmt.Sprintf("ACL DELUSER %s\nACL SAVE\n", l.Role)); err != nil {
			return "", err
		}
	case kind == "redis":
		return "", fmt.Errorf("%s's link predates per-app Redis users, so it can't be revoked on its own; run `berth redis:link %s %s` first, then unlink", app, name, app)
	}
	delete(sv.Links, app)
	return l.EnvVar, m.Store.SaveService(sv)
}

// Destroy removes the container and, unless keepData, the volume.
func (m *Manager) Destroy(ctx context.Context, kind, name string, keepData bool) error {
	l := m.lock(kind, name)
	l.Lock()
	defer l.Unlock()
	sv, err := m.Store.Service(kind, name)
	if err != nil {
		return err
	}
	m.RT.Remove(ctx, state.ServiceContainer(kind, name))
	if !keepData {
		if err := m.RT.VolumeRemove(ctx, sv.Volume); err != nil && m.RT.VolumeExists(ctx, sv.Volume) {
			return err
		}
	}
	return m.Store.DeleteService(kind, name)
}

// Backup writes a dump to backups/<kind>/<name>/<timestamp>.<ext> and
// prunes beyond keep (0 = no pruning).
func (m *Manager) Backup(ctx context.Context, kind, name string, keep int) (string, error) {
	sv, err := m.Store.Service(kind, name)
	if err != nil {
		return "", err
	}
	dir := m.Store.Path("backups", kind, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ext := "dump"
	cmd := []string{"pg_dump", "-Fc", "-U", "postgres", dbName(name)}
	if kind == "redis" {
		ext = "rdb"
		cmd = []string{"sh", "-c", `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning SAVE >/dev/null && cat /data/dump.rdb`}
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+"."+ext)
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	err = m.RT.Exec(ctx, state.ServiceContainer(kind, sv.Name), cmd, false, false, nil, f, nil)
	f.Close()
	if err != nil {
		os.Remove(path + ".tmp")
		return "", fmt.Errorf("backup %s:%s: %w", kind, name, err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return "", err
	}
	if keep > 0 {
		Prune(dir, keep)
	}
	return path, nil
}

// Prune keeps the newest keep files in dir.
func Prune(dir string, keep int) int {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	removed := 0
	for len(names) > keep {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
		removed++
	}
	return removed
}

// LatestBackup returns the newest backup file for a service, or "".
func (m *Manager) LatestBackup(kind, name string) string {
	entries, _ := os.ReadDir(m.Store.Path("backups", kind, name))
	latest := ""
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") && e.Name() > latest {
			latest = e.Name()
		}
	}
	if latest == "" {
		return ""
	}
	return m.Store.Path("backups", kind, name, latest)
}

// Restore loads a dump into the service.
func (m *Manager) Restore(ctx context.Context, kind, name, file string) error {
	l := m.lock(kind, name)
	l.Lock()
	defer l.Unlock()
	return m.restore(ctx, kind, name, file)
}

func (m *Manager) restore(ctx context.Context, kind, name, file string) error {
	sv, err := m.Store.Service(kind, name)
	if err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	container := state.ServiceContainer(kind, name)
	switch kind {
	case "pg":
		var out bytes.Buffer
		err := m.RT.Exec(ctx, container, []string{"pg_restore", "-U", "postgres", "-d", dbName(name), "--clean", "--if-exists", "--no-owner", "--role", dbName(name) + "_rw", "--exit-on-error"}, true, false, f, &out, nil)
		if err != nil && strings.Contains(out.String()+err.Error(), "role \""+dbName(name)+"_rw\" does not exist") {
			// No links yet: restore owned by postgres.
			f.Seek(0, 0)
			out.Reset()
			err = m.RT.Exec(ctx, container, []string{"pg_restore", "-U", "postgres", "-d", dbName(name), "--clean", "--if-exists", "--no-owner", "--exit-on-error"}, true, false, f, &out, nil)
		}
		if err != nil {
			return fmt.Errorf("pg_restore: %v %s", err, strings.TrimSpace(out.String()))
		}
		return nil
	case "redis":
		cfg, err := m.Store.Config()
		if err != nil {
			return err
		}
		m.RT.Stop(ctx, container, 10*time.Second)
		m.RT.Remove(ctx, container)
		// In a throwaway container on the stopped service's volume: load the
		// RDB with AOF off, then turn AOF on so Redis writes a fresh AOF from
		// memory (the service runs with --appendonly yes, which would
		// otherwise ignore the RDB and start empty).
		rerr := m.RT.RunWith(ctx, runtime.RunOpts{
			Image:       sv.Image,
			Remove:      true,
			Interactive: true,
			Volumes:     []string{sv.Volume + ":/data"},
			Entrypoint:  "sh",
			Args:        []string{"-c", redisRestoreScript},
		}, f, io.Discard)
		if err := m.run(ctx, sv, cfg); err != nil {
			return err
		}
		if rerr != nil {
			return rerr
		}
		return m.WaitReady(ctx, sv, 60*time.Second)
	}
	return fmt.Errorf("restore not supported for %s", kind)
}

const redisRestoreScript = `set -e
rm -rf /data/appendonlydir /data/dump.rdb
cat > /data/dump.rdb
redis-server --dir /data --appendonly no --daemonize yes --logfile /tmp/restore.log
until redis-cli ping 2>/dev/null | grep -q PONG; do sleep 0.2; done
redis-cli CONFIG SET appendonly yes >/dev/null
sleep 0.5
until redis-cli INFO persistence | grep -q aof_rewrite_in_progress:0 && ls /data/appendonlydir/*.base.* >/dev/null 2>&1; do sleep 0.2; done
redis-cli SHUTDOWN NOSAVE >/dev/null 2>&1 || true
`

// Expose publishes the service on 127.0.0.1:port (0 removes it).
func (m *Manager) Expose(ctx context.Context, kind, name string, port int) error {
	l := m.lock(kind, name)
	l.Lock()
	defer l.Unlock()
	sv, err := m.Store.Service(kind, name)
	if err != nil {
		return err
	}
	cfg, err := m.Store.Config()
	if err != nil {
		return err
	}
	sv.ExposePort = port
	if err := m.Store.SaveService(sv); err != nil {
		return err
	}
	if err := m.run(ctx, sv, cfg); err != nil {
		return err
	}
	return m.WaitReady(ctx, sv, 60*time.Second)
}

// Upgrade moves a Postgres service to a new major version: dump, new
// container on a new volume, re-create roles, restore. The old volume is
// kept and its name returned so the user can delete it once satisfied.
func (m *Manager) Upgrade(ctx context.Context, name, version string, out io.Writer) (string, error) {
	l := m.lock("pg", name)
	l.Lock()
	defer l.Unlock()
	sv, err := m.Store.Service("pg", name)
	if err != nil {
		return "", err
	}
	cfg, err := m.Store.Config()
	if err != nil {
		return "", err
	}
	newImage := "postgres:" + version
	if newImage == sv.Image {
		return "", fmt.Errorf("pg:%s is already on %s", name, newImage)
	}
	fmt.Fprintf(out, "-----> Dumping pg:%s\n", name)
	dump, err := m.Backup(ctx, "pg", name, 0)
	if err != nil {
		return "", err
	}
	oldVolume, oldImage := sv.Volume, sv.Image
	sv.Image = newImage
	sv.Volume = fmt.Sprintf("berth-pg-%s-v%s", name, strings.ReplaceAll(version, ".", "-"))
	fmt.Fprintf(out, "-----> Starting %s on volume %s\n", newImage, sv.Volume)
	if err := m.RT.VolumeCreate(ctx, sv.Volume); err != nil {
		return "", err
	}
	if err := m.run(ctx, sv, cfg); err != nil {
		return "", m.rollbackUpgrade(ctx, sv, oldImage, oldVolume, cfg, err)
	}
	if err := m.WaitReady(ctx, sv, 120*time.Second); err != nil {
		return "", m.rollbackUpgrade(ctx, sv, oldImage, oldVolume, cfg, err)
	}
	for _, l := range sv.Links {
		if err := m.applyGrants(ctx, sv, l); err != nil {
			return "", m.rollbackUpgrade(ctx, sv, oldImage, oldVolume, cfg, err)
		}
	}
	fmt.Fprintf(out, "-----> Restoring %s\n", filepath.Base(dump))
	if err := m.restore(ctx, "pg", name, dump); err != nil {
		return "", m.rollbackUpgrade(ctx, sv, oldImage, oldVolume, cfg, err)
	}
	if err := m.Store.SaveService(sv); err != nil {
		return "", err
	}
	return oldVolume, nil
}

func (m *Manager) rollbackUpgrade(ctx context.Context, sv *state.Service, image, volume string, cfg *state.Config, cause error) error {
	failed := sv.Volume
	sv.Image, sv.Volume = image, volume
	m.run(ctx, sv, cfg)
	m.WaitReady(ctx, sv, 60*time.Second)
	m.RT.VolumeRemove(ctx, failed)
	return fmt.Errorf("upgrade failed, kept %s: %w", image, cause)
}

// RelinkAll refreshes every Postgres role (after a restore onto a fresh
// volume, roles are missing).
func (m *Manager) RelinkAll(ctx context.Context, sv *state.Service) error {
	for _, l := range sv.Links {
		var err error
		switch sv.Kind {
		case "pg":
			err = m.applyGrants(ctx, sv, l)
		case "redis":
			err = m.applyRedisUser(ctx, sv, l)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
