package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/runtime"
)

func commitFiles(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "workers"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
}

func TestCronOnTheDaemon(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	commitFiles(t, repo, map[string]string{
		"Procfile": "worker: node worker.js\n",
		"app.json": `{"cron":[{"command":"node digest.js","schedule":"30 7 * * *"},{"command":"node fail.js","schedule":"@hourly"}]}`,
	})
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, params, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// Cron: the 07:30 job fires at 07:30 only; failures are logged and evented.
	rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error {
		io.WriteString(stdout, "ran "+o.Args[1]+"\n")
		if strings.Contains(o.Args[1], "fail") {
			return errors.New("exit status 1")
		}
		return nil
	}
	d.fireCron(d.ctx, time.Date(2026, 10, 25, 7, 29, 0, 0, time.Local))
	d.fireCron(d.ctx, time.Date(2026, 10, 25, 7, 30, 0, 0, time.Local))
	waitFor(t, func() bool { return len(rt.CallLog("runwith berth/")) >= 1 })
	d.fireCron(d.ctx, time.Date(2026, 10, 25, 8, 0, 0, 0, time.Local))
	waitFor(t, func() bool {
		b, _ := os.ReadFile(d.Store.Path("apps", "web", "logs", "cron.log"))
		return strings.Contains(string(b), "cron.2: -----> failed")
	})
	if n := len(rt.CallLog("runwith berth/")); n != 2 {
		t.Fatalf("%d runs: %v", n, rt.CallLog("runwith berth/"))
	}
	logb, _ := os.ReadFile(d.Store.Path("apps", "web", "logs", "cron.log"))
	if !strings.Contains(string(logb), " v1 cron.1: ran node digest.js") {
		t.Fatalf("cron.log:\n%s", logb)
	}
	waitFor(t, func() bool {
		evs, _ := os.ReadFile(d.Store.EventsPath())
		return strings.Contains(string(evs), `"kind":"cron.failed"`)
	})

	lp, _ := json.Marshal(CronParams{App: "web"})
	res, err := d.hCronList(d.ctx, lp, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	list := res.(CronList)
	if len(list.Jobs) != 2 || list.Jobs[0].Last == nil || list.Jobs[0].Last.Error != "" || list.Jobs[1].Last.Error == "" || list.Jobs[0].Next.Hour() != 7 {
		t.Fatalf("list %+v", list)
	}
	// By hand, with the output streamed back.
	var out bytes.Buffer
	rp, _ := json.Marshal(CronParams{App: "web", Index: 1})
	if _, err := d.hCronRun(d.ctx, rp, &out); err != nil || !strings.Contains(out.String(), "ran node digest.js") {
		t.Fatalf("cron:run: %v\n%s", err, out.String())
	}
	// A job that keeps failing notifies once, and again when it recovers.
	fp, _ := json.Marshal(CronParams{App: "web", Index: 2})
	for i := 0; i < 2; i++ {
		if _, err := d.hCronRun(d.ctx, fp, io.Discard); err == nil {
			t.Fatal("fail.js succeeded")
		}
	}
	rt.RunWithHook = func(o runtime.RunOpts, stdout io.Writer) error { return nil }
	if _, err := d.hCronRun(d.ctx, fp, io.Discard); err != nil {
		t.Fatal(err)
	}
	evlog, _ := os.ReadFile(d.Store.EventsPath())
	if n := strings.Count(string(evlog), `"kind":"cron.failed"`); n != 1 {
		t.Fatalf("%d cron.failed events for 3 failures in a row:\n%s", n, evlog)
	}
	if !strings.Contains(string(evlog), "succeeded again after 3 failed runs") {
		t.Fatalf("no recovery event:\n%s", evlog)
	}

	rp, _ = json.Marshal(CronParams{App: "web", Index: 3})
	if _, err := d.hCronRun(d.ctx, rp, &out); err == nil {
		t.Fatal("ran job 3 of 2")
	}

	// Stopped apps and dev versions don't run schedules.
	before := len(rt.CallLog("runwith berth/"))
	d.Deployer.Stop(d.ctx, "web")
	d.fireCron(d.ctx, time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local))
	time.Sleep(50 * time.Millisecond)
	if n := len(rt.CallLog("runwith berth/")); n != before {
		t.Fatalf("a stopped app ran cron (%d → %d)", before, n)
	}
	d.stopTails()
	time.Sleep(50 * time.Millisecond)
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
