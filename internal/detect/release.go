package detect

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dps/berth/internal/cron"
)

// Procfile reads dir's Procfile into process type → command.
func Procfile(dir string) map[string]string {
	f, err := os.Open(filepath.Join(dir, "Procfile"))
	if err != nil {
		return nil
	}
	defer f.Close()
	procs := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if name, cmd, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(cmd) != "" {
			procs[strings.TrimSpace(name)] = strings.TrimSpace(cmd)
		}
	}
	return procs
}

// ProcfileRelease returns the command on the Procfile's `release:` line in
// dir, or "".
func ProcfileRelease(dir string) string { return Procfile(dir)["release"] }

// ProcfileWorkers returns the Procfile's process types other than web and
// release, which Berth runs as workers. The image's CMD is the web process.
// Types with names Berth can't use as container names are reported in bad.
func ProcfileWorkers(dir string) (workers map[string]string, bad []string) {
	for name, cmd := range Procfile(dir) {
		if name == "web" || name == "release" {
			continue
		}
		if !processNameRe.MatchString(name) {
			bad = append(bad, name)
			continue
		}
		if workers == nil {
			workers = map[string]string{}
		}
		workers[name] = cmd
	}
	sort.Strings(bad)
	return workers, bad
}

var processNameRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,19}$`)

// CronJob is one entry of app.json's "cron" list (Dokku's format).
type CronJob struct {
	Command  string `json:"command"`
	Schedule string `json:"schedule"`
}

// AppJSONCron reads the "cron" list from dir's app.json. A missing file or
// list is no jobs; a malformed one is an error, so a typo doesn't silently
// drop a schedule.
func AppJSONCron(dir string) ([]CronJob, error) {
	b, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var aj struct {
		Cron []CronJob `json:"cron"`
	}
	if err := json.Unmarshal(b, &aj); err != nil {
		return nil, fmt.Errorf("app.json: %w", err)
	}
	for i, j := range aj.Cron {
		if strings.TrimSpace(j.Command) == "" {
			return nil, fmt.Errorf("app.json: cron entry %d has no command", i+1)
		}
		if _, err := cron.Parse(j.Schedule); err != nil {
			return nil, fmt.Errorf("app.json: cron entry %d: %w", i+1, err)
		}
	}
	return aj.Cron, nil
}

// MigrationHint suggests a release command for a repo that looks like it
// uses a migration tool, or returns "". Berth only prints the hint: running
// a migration nobody asked for is not its call.
func MigrationHint(dir string) (tool, command string) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	glob := func(pattern string) bool {
		m, _ := filepath.Glob(filepath.Join(dir, pattern))
		return len(m) > 0
	}
	switch {
	case has("prisma/schema.prisma") && has("prisma/migrations"):
		return "Prisma", "npx prisma migrate deploy"
	case has("drizzle.config.ts") || has("drizzle.config.js"):
		return "Drizzle", "npx drizzle-kit migrate"
	case glob("knexfile.*"):
		return "Knex", "npx knex migrate:latest"
	case has("alembic.ini"):
		return "Alembic", "alembic upgrade head"
	case has("manage.py") && glob("*/migrations/__init__.py"):
		return "Django", "python manage.py migrate --noinput"
	case has("bin/rails") && has("db/migrate"):
		return "Rails", "bin/rails db:migrate"
	case has("migrate.js"):
		return "migrate.js", "node migrate.js"
	}
	return "", ""
}
