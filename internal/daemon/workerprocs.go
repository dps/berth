package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// ---- workers: the Procfile's other process types (design.md, Workers and cron) ----

// Workers run as more processes in the app's live web container
// (container exec), as cron jobs for SQLite apps do: they share its build,
// config, volumes and memory, an app with SQLite can open its database from
// them, and an app is one VM however many workers it has. berthd keeps each
// exec attached to copy its output into app.log, restarts it with the web
// process's backoff when it exits, and moves it to the new container when a
// release goes live. Each process carries BERTH_PROCESS=<worker.1> in its
// environment, which is how Berth finds it again to stop it: killing the
// exec client outright leaves the process running inside, as can a berthd
// that dies.

// workerProc is one worker process inside a web container.
type workerProc struct {
	app, name, container, command string
	release                       int
	cancel                        context.CancelFunc
	done                          chan struct{} // closed when its loop ends
	stop                          chan struct{} // closed when Berth stops it
	stopping                      atomic.Bool

	mu       sync.Mutex
	since    time.Time // the current run's start; zero while it's down
	failures int
	tail     []string // its last lines, for a deploy's warning
}

func (w *workerProc) status() (running bool, since time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.since.IsZero(), w.since
}

// workerProcs is the daemon's set of them, keyed app/worker.1.
type workerProcs struct {
	mu    sync.Mutex
	m     map[string]*workerProc
	stops sync.WaitGroup // stopWorker calls under way
}

// workerScript starts ($1 = start, $2 = worker.1, $3 = command) or
// signals ($1 = TERM or KILL, $2 = worker.1) a worker inside the
// container. Starting first stops any copy already there.
const workerScript = `mode=$1 name=$2
sig=$mode; [ "$mode" = start ] && sig=TERM
found=
for e in /proc/[0-9]*/environ; do
  p=${e#/proc/}; p=${p%/environ}
  [ "$p" = "$$" ] && continue
  if tr '\0' '\n' < "$e" 2>/dev/null | grep -qx "BERTH_PROCESS=$name"; then
    kill -s "$sig" "$p" 2>/dev/null && found=1
  fi
done
[ "$mode" = start ] || { [ -n "$found" ]; exit; }
[ -n "$found" ] && sleep 2
export BERTH_PROCESS="$name"
exec /bin/sh -c "$3"`

var (
	// workerStopWait is how long a worker gets after TERM before KILL.
	workerStopWait = 10 * time.Second
	// workerSettle is how long a deploy waits before checking that the
	// workers it started are still running.
	workerSettle = 3 * time.Second
)

// syncAllWorkers is the supervisor's pass: every app's workers match its
// live container and scale. Apps mid-deploy are left to the deploy.
func (d *Daemon) syncAllWorkers(ctx context.Context, apps []*state.App, cs []runtime.Container) {
	have := map[string]bool{}
	for _, a := range apps {
		have[a.Name] = true
		if !d.Deployer.Busy(a.Name) {
			d.syncWorkers(ctx, a, cs, nil)
		}
	}
	d.wp.mu.Lock()
	for key, w := range d.wp.m {
		if !have[w.app] { // destroyed
			delete(d.wp.m, key)
			d.wp.stops.Go(func() { d.stopWorker(w) })
		}
	}
	d.wp.mu.Unlock()
}

// appWorkers is the deploy's hook (Deployer.Workers).
func (d *Daemon) appWorkers(ctx context.Context, appName string, out io.Writer) {
	a, err := d.Store.App(appName)
	if err != nil {
		return
	}
	cs, err := d.RT.List(ctx)
	if err != nil {
		return
	}
	d.syncWorkers(ctx, a, cs, out)
}

// syncWorkers starts, stops and moves a's workers to match its current
// release, its scale and whether that release's container is running. A
// worker moving to a new container stops in the old one first, so a job is
// never picked up twice. With out, it reports what it started and waits
// workerSettle to warn about any that exited straight away.
func (d *Daemon) syncWorkers(ctx context.Context, a *state.App, cs []runtime.Container, out io.Writer) {
	type want struct {
		container, command string
		release            int
	}
	wants := map[string]want{}
	if rel := d.Store.CurrentRelease(a.Name); rel != nil {
		d.Deployer.RemoveWorkerContainers(ctx, a.Name, rel) // an older Berth's
		c, ok := runtime.Find(cs, rel.Container)
		if !a.Stopped && rel.Container != "" && ok && c.Running() {
			for t, cmd := range rel.Processes {
				for i := 1; i <= a.WorkerCount(t); i++ {
					w := state.Worker{Type: t, Index: i}
					wants[a.Name+"/"+w.Name()] = want{rel.Container, cmd, rel.N}
				}
			}
		}
	}

	d.wp.mu.Lock()
	if d.wp.m == nil {
		d.wp.m = map[string]*workerProc{}
	}
	stopped := map[string]chan struct{}{}
	for key, w := range d.wp.m {
		if w.app != a.Name {
			continue
		}
		if nw, ok := wants[key]; ok && nw.container == w.container && nw.command == w.command {
			continue
		}
		delete(d.wp.m, key)
		stopped[key] = w.done
		d.wp.stops.Go(func() { d.stopWorker(w) })
	}
	var started []*workerProc
	for key, nw := range wants {
		if _, ok := d.wp.m[key]; ok {
			continue
		}
		_, name, _ := strings.Cut(key, "/")
		wctx, cancel := context.WithCancel(ctx)
		w := &workerProc{app: a.Name, name: name, container: nw.container, command: nw.command, release: nw.release,
			cancel: cancel, done: make(chan struct{}), stop: make(chan struct{})}
		d.wp.m[key] = w
		started = append(started, w)
		go d.runWorker(wctx, w, stopped[key])
	}
	d.wp.mu.Unlock()

	if out == nil || len(started) == 0 {
		return
	}
	sortWorkers(started)
	var names []string
	for _, w := range started {
		names = append(names, w.name)
	}
	fmt.Fprintf(out, "-----> Started %s\n", strings.Join(names, ", "))
	// One that exits straight away has a config or code problem worth
	// showing now rather than only in the logs.
	time.Sleep(workerSettle)
	for _, w := range started {
		w.mu.Lock()
		down := w.failures > 0
		tail := append([]string(nil), w.tail...)
		w.mu.Unlock()
		if down {
			fmt.Fprintf(out, "       warning: %s exited right after starting; Berth will keep restarting it. Its last lines:\n", w.name)
			for _, l := range tail {
				fmt.Fprintf(out, "         %s\n", l)
			}
		}
	}
}

// runWorker runs w until Berth stops it, restarting it when it exits. It
// starts once after (the same worker stopping in the old container) is
// done.
func (d *Daemon) runWorker(ctx context.Context, w *workerProc, after <-chan struct{}) {
	defer close(w.done)
	if after != nil {
		select {
		case <-after:
		case <-w.stop:
			return
		}
	}
	log := NewRotatingWriter(d.Store.Path("apps", w.app, "logs", "app.log"), 50<<20, 5)
	defer log.Close()
	lines := &lineWriter{w: log, prefix: fmt.Sprintf("v%d %s:", w.release, w.name), keep: w}
	for {
		start := time.Now()
		w.mu.Lock()
		w.since = start
		w.mu.Unlock()
		err := d.RT.Exec(ctx, w.container, []string{"/bin/sh", "-c", workerScript, "berth-worker", "start", w.name, w.command}, false, false, nil, lines, lines)
		lines.flush()
		w.mu.Lock()
		w.since = time.Time{}
		if time.Since(start) > crashResetAfter {
			w.failures = 0
		}
		w.failures++
		failures := w.failures
		w.mu.Unlock()
		if ctx.Err() != nil || w.stopping.Load() {
			return
		}
		// The container stopping (a deploy, a restart, an idle dev
		// version) ends its workers; syncWorkers moves or drops them.
		if cs, lerr := d.RT.List(ctx); lerr == nil {
			if c, ok := runtime.Find(cs, w.container); !ok || !c.Running() {
				select {
				case <-w.stop:
					return
				case <-ctx.Done():
					return
				}
			}
		}
		how := "exited"
		if m := exitCodeRe.FindStringSubmatch(fmt.Sprint(err)); m != nil {
			how = "exited with status " + m[1]
		}
		if failures > maxCrashFailures {
			lines.say("%s %d times in a row; not restarting it until the next deploy or `berth restart %s`", how, maxCrashFailures, w.app)
			d.event(state.Event{Kind: "crash.loop", App: w.app, Message: fmt.Sprintf("%s (v%d) exited %d times in a row; Berth stopped restarting it until the next deploy or `berth restart %s` (see `berth logs %s`)", w.name, w.release, maxCrashFailures, w.app, w.app)})
			select {
			case <-w.stop:
			case <-ctx.Done():
			}
			return
		}
		wait := d.CrashBackoff(failures)
		lines.say("%s; restarting in %s", how, wait)
		d.event(state.Event{Kind: "crash", App: w.app, Message: fmt.Sprintf("%s (v%d) %s; restarting (%d of %d before Berth gives up)", w.name, w.release, how, failures, maxCrashFailures)})
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// stopWorker sends w's process TERM, then KILL if it hasn't exited in
// workerStopWait, and waits for its loop to end.
func (d *Daemon) stopWorker(w *workerProc) {
	ctx, cancel := context.WithTimeout(context.Background(), workerStopWait+30*time.Second)
	defer cancel()
	w.stopping.Store(true)
	close(w.stop)
	signal := func(sig string) {
		if err := d.RT.Exec(ctx, w.container, []string{"/bin/sh", "-c", workerScript, "berth-worker", sig, w.name}, false, false, nil, io.Discard, io.Discard); err != nil {
			slog.Debug("worker signal", "app", w.app, "worker", w.name, "sig", sig, "err", err)
		}
	}
	running, _ := w.status()
	if running {
		signal("TERM")
		select {
		case <-w.done:
		case <-time.After(workerStopWait):
			signal("KILL")
		}
	}
	w.cancel()
	<-w.done
}

// stopAllWorkers stops every worker (daemon shutdown leaves them running,
// to be replaced by the next berthd; this is for tests).
func (d *Daemon) stopAllWorkers() {
	d.wp.mu.Lock()
	ws := d.wp.m
	d.wp.m = nil
	d.wp.mu.Unlock()
	for _, w := range ws {
		d.wp.stops.Go(func() { d.stopWorker(w) })
	}
	d.wp.stops.Wait()
}

// workerStatus is app's workers, for status.
func (d *Daemon) workerStatus(app string) []ProcessStatus {
	d.wp.mu.Lock()
	var ws []*workerProc
	for _, w := range d.wp.m {
		if w.app == app {
			ws = append(ws, w)
		}
	}
	d.wp.mu.Unlock()
	sortWorkers(ws)
	var out []ProcessStatus
	for _, w := range ws {
		ps := ProcessStatus{Name: w.name, Command: w.command}
		ps.Running, ps.Since = w.status()
		out = append(out, ps)
	}
	return out
}

// sortWorkers orders worker.2 before worker.10.
func sortWorkers(ws []*workerProc) {
	sort.Slice(ws, func(i, j int) bool {
		ti, ni, _ := strings.Cut(ws[i].name, ".")
		tj, nj, _ := strings.Cut(ws[j].name, ".")
		if ti != tj {
			return ti < tj
		}
		a, _ := strconv.Atoi(ni)
		b, _ := strconv.Atoi(nj)
		return a < b
	})
}

// lineWriter writes each line with a timestamp and prefix, as tail does,
// keeping the last few on keep.
type lineWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
	keep   *workerProc
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.line(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 1<<20 {
		l.line(l.buf)
		l.buf = nil
	}
	return len(p), nil
}

func (l *lineWriter) line(b []byte) {
	fmt.Fprintf(l.w, "%s %s %s\n", time.Now().UTC().Format(time.RFC3339), l.prefix, b)
	if l.keep != nil {
		l.keep.mu.Lock()
		l.keep.tail = append(l.keep.tail, string(b))
		if len(l.keep.tail) > 20 {
			l.keep.tail = l.keep.tail[1:]
		}
		l.keep.mu.Unlock()
	}
}

func (l *lineWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.line(l.buf)
		l.buf = nil
	}
}

// say writes a line of Berth's own about the worker.
func (l *lineWriter) say(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.line([]byte("[berth] " + fmt.Sprintf(format, args...)))
}
