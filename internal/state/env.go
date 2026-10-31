package state

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Env is an app's live config: apps/<app>/env, KEY=value per line.
type Env map[string]string

func (s *Store) Env(app string) (Env, error) {
	f, err := os.Open(s.Path("apps", app, "env"))
	if err != nil {
		if os.IsNotExist(err) {
			return Env{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return ParseEnv(f)
}

func (s *Store) SaveEnv(app string, e Env) error {
	return atomicWrite(s.Path("apps", app, "env"), []byte(e.Format()), 0o600)
}

// ParseEnv reads KEY=value lines, ignoring blanks and # comments. Values may
// be double- or single-quoted (as in .env files); `export ` prefixes are
// accepted.
func ParseEnv(r interface{ Read([]byte) (int, error) }) (Env, error) {
	e := Env{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=value", line)
		}
		k = strings.TrimSpace(k)
		if err := ValidateEnvKey(k); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		e[k] = unquote(strings.TrimSpace(v))
	}
	return e, sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 {
		if v[0] == '"' && v[len(v)-1] == '"' {
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return v[1 : len(v)-1]
		}
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func ValidateEnvKey(k string) error {
	if k == "" {
		return fmt.Errorf("empty variable name")
	}
	for i, c := range k {
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return fmt.Errorf("invalid variable name %q", k)
	}
	return nil
}

func (e Env) Keys() []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Format renders the env file. Values containing newlines or quotes are
// Go-quoted so ParseEnv round-trips them.
func (e Env) Format() string {
	var b strings.Builder
	for _, k := range e.Keys() {
		v := e[k]
		if strings.ContainsAny(v, "\n\r\"") || strings.HasPrefix(v, "'") || strings.TrimSpace(v) != v {
			v = strconv.Quote(v)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String()
}

// ContainerEnvFile renders the env for `container run --env-file`, which takes
// values literally (no quote processing), so multi-line values are dropped
// from the file and returned separately to be passed with -e.
func (e Env) ContainerEnvFile() (file string, extra []string) {
	var b strings.Builder
	for _, k := range e.Keys() {
		v := e[k]
		if strings.ContainsAny(v, "\n\r") {
			extra = append(extra, k+"="+v)
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String(), extra
}

func (e Env) Clone() Env {
	c := Env{}
	for k, v := range e {
		c[k] = v
	}
	return c
}

// IsSecretKey reports whether config:show should mask the value.
func IsSecretKey(k string) bool {
	u := strings.ToUpper(k)
	for _, word := range strings.Split(u, "_") {
		switch word {
		case "KEY", "APIKEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "PASS", "PWD", "AUTH",
			"CREDENTIAL", "CREDENTIALS", "PRIVATE", "DSN", "COOKIE", "SALT":
			return true
		}
	}
	return strings.HasSuffix(u, "PASSWORD")
}

// IsSecretValue catches credentials in values whatever the key is called,
// such as URLs with a password (postgres://user:pw@host/db).
func IsSecretValue(v string) bool {
	if i := strings.Index(v, "://"); i > 0 {
		if at := strings.Index(v[i+3:], "@"); at > 0 && strings.Contains(v[i+3:i+3+at], ":") {
			return true
		}
	}
	return strings.Contains(v, "-----BEGIN ")
}

func Mask(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return v[:2] + strings.Repeat("*", 6) + v[len(v)-2:]
}

// Berth-controlled settings, read from the app env.
const (
	KeyDockerfile     = "BERTH_DOCKERFILE"
	KeyHealthcheck    = "BERTH_HEALTHCHECK"
	KeyHealthTimeout  = "BERTH_HEALTH_TIMEOUT"
	KeyDrain          = "BERTH_DRAIN"
	KeyMemory         = "BERTH_MEMORY"
	KeyCPUs           = "BERTH_CPUS"
	KeyKeepReleases   = "BERTH_KEEP_RELEASES"
	KeyRelease        = "BERTH_RELEASE_COMMAND"
	KeyReleaseTimeout = "BERTH_RELEASE_TIMEOUT"
	KeyReleaseBackup  = "BERTH_RELEASE_BACKUP"
	KeyCronTimeout    = "BERTH_CRON_TIMEOUT"
	KeyPort           = "PORT"
)

// IsSetting reports whether key configures Berth rather than the app. Apps
// don't see these. BERTH_TOOLS_* are the exception: they're how an app
// reaches the tools gateway.
func IsSetting(key string) bool {
	return strings.HasPrefix(key, "BERTH_") && !strings.HasPrefix(key, "BERTH_TOOLS_")
}

// IntSetting reads an integer BERTH_* setting with a default.
func (e Env) IntSetting(key string, def int) int {
	if v, ok := e[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func (e Env) StrSetting(key, def string) string {
	if v, ok := e[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}
