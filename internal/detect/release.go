package detect

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ProcfileRelease returns the command on the Procfile's `release:` line in
// dir, or "". Other process types are ignored: the image's CMD is the web
// process.
func ProcfileRelease(dir string) string {
	f, err := os.Open(filepath.Join(dir, "Procfile"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if name, cmd, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "release" {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
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
