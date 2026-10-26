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

func TestProcfileWorkersAndCron(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Procfile"), []byte("web: node server.js\nrelease: npm run migrate\nworker: node worker.js\nclock: node clock.js\nBad_Name: x\n"), 0o644)
	w, bad := ProcfileWorkers(dir)
	if len(w) != 2 || w["worker"] != "node worker.js" || w["clock"] != "node clock.js" || len(bad) != 1 || bad[0] != "Bad_Name" {
		t.Fatalf("workers %v, bad %v", w, bad)
	}
	if jobs, err := AppJSONCron(dir); err != nil || jobs != nil {
		t.Fatalf("no app.json: %v %v", jobs, err)
	}
	os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"name":"x","cron":[{"command":"npm run digest","schedule":"0 7 * * *"},{"command":"node sweep.js","schedule":"@hourly"}]}`), 0o644)
	jobs, err := AppJSONCron(dir)
	if err != nil || len(jobs) != 2 || jobs[1].Schedule != "@hourly" {
		t.Fatalf("jobs %v %v", jobs, err)
	}
	os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"cron":[{"command":"x","schedule":"every day"}]}`), 0o644)
	if _, err := AppJSONCron(dir); err == nil {
		t.Fatal("bad schedule accepted")
	}
}
