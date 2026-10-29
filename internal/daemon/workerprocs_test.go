package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dps/berth/internal/runtime"
)

// fakeWorkers stands in for workers exec'd into containers: a started one
// runs until it's sent TERM or its container stops, as with Apple's exec.
type fakeWorkers struct {
	mu       sync.Mutex
	rt       *runtime.Fake
	running  map[string]chan struct{} // container/worker.1
	starts   []string
	failNext map[string]bool
}

func (f *fakeWorkers) hook(name string, args []string, stdout io.Writer) error {
	if len(args) < 6 || args[3] != "berth-worker" {
		return nil
	}
	mode, worker, key := args[4], args[5], name+"/"+args[5]
	f.mu.Lock()
	if mode != "start" {
		if ch, ok := f.running[key]; ok {
			close(ch)
			delete(f.running, key)
		}
		f.mu.Unlock()
		return nil
	}
	f.starts = append(f.starts, key)
	if f.failNext[worker] {
		delete(f.failNext, worker)
		f.mu.Unlock()
		fmt.Fprintln(stdout, "boom")
		return fmt.Errorf("exit status 3")
	}
	ch := make(chan struct{})
	f.running[key] = ch
	f.mu.Unlock()
	fmt.Fprintf(stdout, "%s up\n", worker)
	for {
		select {
		case <-ch:
			return fmt.Errorf("exit status 143")
		case <-time.After(5 * time.Millisecond):
		}
		cs, _ := f.rt.List(context.Background())
		if c, ok := runtime.Find(cs, name); !ok || !c.Running() {
			f.mu.Lock()
			if f.running[key] == ch {
				delete(f.running, key)
			}
			f.mu.Unlock()
			return fmt.Errorf("container %s is not running", name)
		}
	}
}

func (f *fakeWorkers) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.running {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 400 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkersRunInsideTheWebContainer(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	d.CrashBackoff = func(int) time.Duration { return 0 }
	defer func(v time.Duration) { workerStopWait, workerSettle = v, 0 }(workerStopWait)
	workerStopWait, workerSettle = 50*time.Millisecond, 50*time.Millisecond
	fw := &fakeWorkers{rt: rt, running: map[string]chan struct{}{}, failNext: map[string]bool{}}
	rt.ExecHook = fw.hook
	defer d.stopAllWorkers()

	commitFiles(t, repo, map[string]string{"Procfile": "worker: python worker.py\n"})
	var out bytes.Buffer
	b, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, b, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Started worker.1") {
		t.Fatalf("deploy output:\n%s", out.String())
	}
	cur := d.Store.CurrentRelease("web")
	sync := func() {
		apps, _ := d.Store.Apps()
		cs, _ := rt.List(d.ctx)
		d.syncAllWorkers(d.ctx, apps, cs)
	}

	eventually(t, "worker.1 in the web container", func() bool { return fw.count(cur.Container+"/") == 1 })
	sync() // a second pass leaves it be
	st, _ := d.Status(d.ctx)
	var ws []ProcessStatus
	for _, a := range st.Apps {
		if a.Name == "web" {
			ws = a.Workers
		}
	}
	if len(ws) != 1 || ws[0].Name != "worker.1" || !ws[0].Running {
		t.Fatalf("status workers %+v", ws)
	}
	eventually(t, "the worker's output in app.log", func() bool {
		b, _ := os.ReadFile(d.Store.Path("apps", "web", "logs", "app.log"))
		return strings.Contains(string(b), fmt.Sprintf("v%d worker.1: worker.1 up", cur.N))
	})

	// Scaling up runs another one inside, which restarts when it exits.
	fw.mu.Lock()
	fw.failNext["worker.2"] = true
	fw.mu.Unlock()
	b, _ = json.Marshal(ScaleParams{App: "web", Type: "worker", Count: 2})
	if _, err := d.hScale(d.ctx, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, c := range rt.CallLog("run ") {
		if strings.Contains(c, "worker") {
			t.Fatalf("scaling ran a container: %s", c)
		}
	}
	eventually(t, "worker.2 restarted", func() bool { return fw.count(cur.Container+"/") == 2 })
	log, _ := os.ReadFile(d.Store.Path("apps", "web", "logs", "app.log"))
	if !strings.Contains(string(log), "worker.2: [berth] exited with status 3; restarting") {
		t.Fatalf("app.log:\n%s", log)
	}

	// A restart moves them to the new container; the old one never has
	// a worker beside the new one's.
	if err := d.Deployer.Restart(d.ctx, "web", io.Discard); err != nil {
		t.Fatal(err)
	}
	next := d.Store.CurrentRelease("web")
	if next.Container == cur.Container {
		t.Fatal("restart kept the container")
	}
	eventually(t, "workers in the new container", func() bool {
		return fw.count(next.Container+"/") == 2 && fw.count(cur.Container+"/") == 0
	})

	// Scaling down and stopping the app stop them.
	b, _ = json.Marshal(ScaleParams{App: "web", Type: "worker", Count: 1})
	d.hScale(d.ctx, b, io.Discard)
	sync()
	eventually(t, "worker.2 stopped", func() bool { return fw.count(next.Container+"/") == 1 })
	a, _ := d.Store.App("web")
	a.Stopped = true
	d.Store.SaveApp(a)
	sync()
	eventually(t, "no workers", func() bool { return fw.count("") == 0 })
	d.wp.mu.Lock()
	left := len(d.wp.m)
	d.wp.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d workers still tracked", left)
	}
}
