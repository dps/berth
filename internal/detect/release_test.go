package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcfileRelease(t *testing.T) {
	dir := t.TempDir()
	if got := ProcfileRelease(dir); got != "" {
		t.Fatalf("no Procfile: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "Procfile"), []byte("web: node server.js\n# comment\nrelease:  npx prisma migrate deploy \nworker: x\n"), 0o644)
	if got := ProcfileRelease(dir); got != "npx prisma migrate deploy" {
		t.Fatalf("got %q", got)
	}
}

func TestMigrationHint(t *testing.T) {
	cases := map[string]struct {
		files []string
		tool  string
	}{
		"prisma":  {[]string{"prisma/schema.prisma", "prisma/migrations/x/migration.sql"}, "Prisma"},
		"alembic": {[]string{"alembic.ini"}, "Alembic"},
		"django":  {[]string{"manage.py", "blog/migrations/__init__.py"}, "Django"},
		"rails":   {[]string{"bin/rails", "db/migrate/1_init.rb"}, "Rails"},
		"knex":    {[]string{"knexfile.js"}, "Knex"},
		"none":    {[]string{"package.json", "prisma/schema.prisma"}, ""},
	}
	for name, c := range cases {
		dir := t.TempDir()
		for _, f := range c.files {
			os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
			os.WriteFile(filepath.Join(dir, f), nil, 0o644)
		}
		if tool, _ := MigrationHint(dir); tool != c.tool {
			t.Errorf("%s: got %q want %q", name, tool, c.tool)
		}
	}
}
