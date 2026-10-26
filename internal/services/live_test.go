package services

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// TestLiveReplace runs Replace against a real, throwaway Postgres in the
// local Berth (BERTH_LIVE=1; it creates and destroys pg:replacetest).
func TestLiveReplace(t *testing.T) {
	if os.Getenv("BERTH_LIVE") == "" {
		t.Skip("BERTH_LIVE not set")
	}
	home, _ := os.UserHomeDir()
	m := &Manager{Store: &state.Store{Root: filepath.Join(home, ".berth")}, RT: runtime.NewApple()}
	ctx := context.Background()
	const name, app = "replacetest", "replacetest-app"
	if _, err := m.Create(ctx, "pg", name, "", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	defer m.Destroy(context.Background(), "pg", name, false)
	if _, _, err := m.Link(ctx, "pg", name, app, "", state.Env{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LinkDev(ctx, name, app+"-dev", "DATABASE_URL", DevDB(name)); err != nil {
		t.Fatal(err)
	}
	sv, _ := m.Store.Service("pg", name)
	// As the app would: its login, with role=replacetest_rw set in the database.
	asApp := func(sql string) string {
		t.Helper()
		var out bytes.Buffer
		err := m.RT.Exec(ctx, state.ServiceContainer("pg", name), []string{"sh", "-c",
			`PGPASSWORD="$1" psql -h 127.0.0.1 -U "$2" -d replacetest -v ON_ERROR_STOP=1 -Atc "$3"`, "sh",
			sv.Links[app].Password, sv.Links[app].Role, sql}, false, false, nil, &out, &out)
		if err != nil {
			t.Fatalf("%s: %v %s", sql, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	asApp("CREATE TABLE notes (id serial PRIMARY KEY, body text); INSERT INTO notes (body) VALUES ('before')")
	dump, err := m.ReleaseBackup(ctx, name, app, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	// "The migration": a new column, a new table, and a write after it.
	asApp("ALTER TABLE notes ADD COLUMN mood text; CREATE TABLE likes (id int); INSERT INTO notes (body) VALUES ('after')")

	safety, err := m.Replace(ctx, name, dump)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(safety); err != nil {
		t.Fatalf("no safety dump: %v", err)
	}
	got := asApp(`SELECT string_agg(body, ','), (SELECT count(*) FROM information_schema.columns WHERE table_name = 'notes' AND column_name = 'mood'), to_regclass('likes') IS NULL, (SELECT tableowner FROM pg_tables WHERE tablename = 'notes') FROM notes`)
	if got != "before|0|t|replacetest_rw" {
		t.Fatalf("after replace: %s", got)
	}
	// The app can still write, and the dev login still can't reach production.
	asApp("INSERT INTO notes (body) VALUES ('again')")
	var out bytes.Buffer
	dev := sv.Links[app+"-dev"]
	err = m.RT.Exec(ctx, state.ServiceContainer("pg", name), []string{"sh", "-c",
		`PGPASSWORD="$1" psql -h 127.0.0.1 -U "$2" -d replacetest -Atc "SELECT 1"`, "sh", dev.Password, dev.Role}, false, false, nil, &out, &out)
	if err == nil || !strings.Contains(err.Error()+out.String(), "CONNECT privilege") {
		t.Fatalf("dev login reached the recreated production database: %v %s", err, out.String())
	}
}
