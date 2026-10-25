package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// releaseHarness records release commands, backups and restores.
type releaseHarness struct {
	*harness
	ran      []runtime.RunOpts
	fail     bool
	backups  []int // release numbers backed up
	restored []map[string]string
}

func newReleaseHarness(t *testing.T) *releaseHarness {
	h := &releaseHarness{harness: newHarness(t)}
	h.rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error {
		h.ran = append(h.ran, o)
		fmt.Fprintf(stdout, "migrating with %s\n", strings.Join(o.Args, " "))
		if h.fail {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}
	dumps := t.TempDir()
	h.d.Backup = func(ctx context.Context, app *state.App, rel *state.Release, out io.Writer) (map[string]string, error) {
		h.backups = append(h.backups, rel.N)
		f := filepath.Join(dumps, fmt.Sprintf("v%d.dump", rel.N))
		os.WriteFile(f, []byte("dump"), 0o600)
		return map[string]string{"pg:db": f}, nil
	}
	h.d.RestoreData = func(ctx context.Context, app *state.App, b map[string]string, out io.Writer) error {
		h.restored = append(h.restored, b)
		return nil
	}
	return h
}

func TestReleaseCommandRunsBeforeSwap(t *testing.T) {
	h := newReleaseHarness(t)
	ctx := context.Background()
	h.commit(map[string]string{"Procfile": "web: ./server\nrelease: ./migrate up\n"})
	var out bytes.Buffer
	rel, err := h.d.Deploy(ctx, Request{App: "web", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 1 {
		t.Fatalf("release command ran %d times", len(h.ran))
	}
	o := h.ran[0]
	if o.Image != rel.Image || o.Entrypoint != "/bin/sh" || strings.Join(o.Args, " ") != "-c ./migrate up" || !o.Remove || o.EnvFile == "" {
		t.Fatalf("release container: %+v", o)
	}
	if rel.ReleaseCmd != "./migrate up" || rel.ProcfileRelease != "./migrate up" {
		t.Fatalf("release record: %+v", rel)
	}
	// It runs before the new container starts.
	calls := strings.Join(h.rt.CallLog("run"), "\n")
	if i, j := strings.Index(calls, "runwith"), strings.Index(calls, "run berth-web-1"); i < 0 || j < i {
		t.Fatalf("release didn't run first:\n%s", calls)
	}
	log, _ := os.ReadFile(h.store.Path("apps", "web", "logs", "release-1.log"))
	if !strings.Contains(string(log), "migrating with -c ./migrate up") || !strings.Contains(out.String(), "migrating") {
		t.Fatalf("release output not logged: %q", log)
	}
	// The first deploy has nothing current, but data still gets backed up.
	if len(h.backups) != 1 || rel.Backups["pg:db"] == "" || rel.DataOf != 0 {
		t.Fatalf("backups %v rel %+v", h.backups, rel)
	}
}

func TestReleaseCommandFailureKeepsOldRelease(t *testing.T) {
	h := newReleaseHarness(t)
	ctx := context.Background()
	r1, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 0 || len(h.backups) != 0 {
		t.Fatal("no release command declared, but one ran")
	}
	h.fail = true
	h.commit(map[string]string{"Procfile": "release: ./migrate up\n"})
	var out bytes.Buffer
	r2, err := h.d.Deploy(ctx, Request{App: "web", Out: &out})
	if err == nil || !strings.Contains(err.Error(), "release command failed") {
		t.Fatalf("want release failure, got %v", err)
	}
	if r2.Status != state.StatusFailed || r2.FailedStage != "release" || h.store.Current("web") != 1 {
		t.Fatalf("v2 %+v current %d", r2, h.store.Current("web"))
	}
	if len(h.rt.CallLog("run berth-web-2")) != 0 {
		t.Fatal("started v2 after its release command failed")
	}
	if !strings.Contains(get(t, r1.HostPort), r1.Container) || !strings.Contains(out.String(), "v1 is still serving") {
		t.Fatalf("v1 should still serve:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "berth pg:restore db ") {
		t.Errorf("output should point at the pre-release backup:\n%s", out.String())
	}
}

func TestReleaseCommandSettingAndConfigReleases(t *testing.T) {
	h := newReleaseHarness(t)
	ctx := context.Background()
	h.commit(map[string]string{"Procfile": "release: ./migrate up\n"})
	if _, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// The setting wins over the Procfile; a config-only release reruns the
	// command but takes no backup (no new code, nothing new to migrate).
	env, _ := h.store.Env("web")
	env[state.KeyRelease] = "./other"
	h.store.SaveEnv("web", env)
	r2, err := h.d.Deploy(ctx, Request{App: "web", ReuseImage: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 2 || h.ran[1].Args[1] != "./other" || r2.ProcfileRelease != "./migrate up" {
		t.Fatalf("ran %+v rel %+v", h.ran, r2)
	}
	if len(h.backups) != 1 || r2.Backups != nil {
		t.Fatalf("config release backed up: %v", h.backups)
	}
	// Removing the setting falls back to the Procfile line, carried over
	// from the built release.
	delete(env, state.KeyRelease)
	env[state.KeyReleaseBackup] = "off"
	h.store.SaveEnv("web", env)
	h.commit(map[string]string{"x": "3"})
	r3, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if r3.ReleaseCmd != "./migrate up" || len(h.backups) != 1 {
		t.Fatalf("BERTH_RELEASE_BACKUP=off: backups %v rel %+v", h.backups, r3)
	}
}

func TestDevVersionReleaseSkipsBackup(t *testing.T) {
	h := newReleaseHarness(t)
	a, _ := h.store.App("web")
	a.DevOf = "parent"
	h.store.SaveApp(a)
	h.commit(map[string]string{"Procfile": "release: ./migrate up\n"})
	if _, err := h.d.Deploy(context.Background(), Request{App: "web", Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 1 || len(h.backups) != 0 {
		t.Fatalf("dev: ran %d backups %v", len(h.ran), h.backups)
	}
}

func TestRollbackWithData(t *testing.T) {
	h := newReleaseHarness(t)
	ctx := context.Background()
	r1, _ := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard}) // no release command
	h.commit(map[string]string{"Procfile": "release: ./migrate up\n"})
	r2, err := h.d.Deploy(ctx, Request{App: "web", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if r2.DataOf != r1.N {
		t.Fatalf("v2's backup should hold v1's data, DataOf=%d", r2.DataOf)
	}
	h.waitRetired(t, r1.Container)
	// v2 → v1 with data restores v2's pre-release backup, after the swap.
	var out bytes.Buffer
	rb, err := h.d.Rollback(ctx, "web", 0, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rb.N != 1 || len(h.restored) != 1 || h.restored[0]["pg:db"] != r2.Backups["pg:db"] {
		t.Fatalf("rolled back to v%d, restored %v", rb.N, h.restored)
	}
	if !strings.Contains(out.String(), "before v2's release command") {
		t.Errorf("output:\n%s", out.String())
	}
	// Back to v2 with data: nothing holds v2's data (no release command ran
	// while it was live), so --with-data refuses before swapping.
	if _, err := h.d.Rollback(ctx, "web", 2, true, io.Discard); err == nil || !strings.Contains(err.Error(), "plain rollback") {
		t.Fatalf("want no-backup error, got %v", err)
	}
	if h.store.Current("web") != 1 {
		t.Fatal("swapped despite the data error")
	}
	// A missing dump is reported, not silently skipped.
	os.Remove(r2.Backups["pg:db"])
	if _, err := h.d.DataFor("web", 1); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("want missing-dump error, got %v", err)
	}
}

func TestErrorLine(t *testing.T) {
	out := "file:///app/migrate.js:40\n  throw new Error(x)\n\nError: 004_broken.sql: relation \"t\" does not exist\n    at file:///app/migrate.js:40:13\n\nNode.js v22.23.4\n"
	if got := errorLine(out); got != `Error: 004_broken.sql: relation "t" does not exist` {
		t.Fatalf("got %q", got)
	}
	if got := errorLine("one\ntwo\n"); got != "two" {
		t.Fatalf("got %q", got)
	}
}
