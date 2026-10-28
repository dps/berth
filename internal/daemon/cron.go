package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/cron"
	"github.com/dps/berth/internal/state"
)

// ---- cron: app.json's scheduled jobs (design.md, Workers and cron) ----

// cronLoop checks every app's current release for jobs due this minute.
// Runs missed while berthd or the Mac was down are skipped, as in cron.
func (d *Daemon) cronLoop(ctx context.Context) {
	last := time.Now().Truncate(time.Minute)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().Truncate(time.Minute)
		if !now.After(last) {
			continue
		}
		last = now
		if d.restoring.Load() {
			continue
		}
		d.fireCron(ctx, time.Now())
	}
}

// fireCron starts every job whose schedule matches now's minute.
func (d *Daemon) fireCron(ctx context.Context, now time.Time) {
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		// Dev versions run jobs only by hand (cron:run): a schedule on a
		// copy of production's data could email real people.
		if a.Stopped || a.DevOf != "" {
			continue
		}
		rel := d.Store.CurrentRelease(a.Name)
		if rel == nil || rel.Status != state.StatusRunning {
			continue
		}
		for i, j := range rel.Cron {
			s, err := cron.Parse(j.Schedule)
			if err != nil || !s.Matches(now) {
				continue
			}
			go d.runCron(ctx, a.Name, rel, i+1, "schedule", nil)
		}
	}
}

// CronRun is the last run of a job, kept in apps/<app>/cron.json by command.
type CronRun struct {
	Started time.Time `json:"started"`
	Secs    float64   `json:"secs"`
	Trigger string    `json:"trigger"`
	Error   string    `json:"error,omitempty"`
	// Failures counts the failed runs in a row, this one included.
	Failures int `json:"failures,omitempty"`
}

var cronStateMu sync.Mutex

func (d *Daemon) cronState(app string) map[string]CronRun {
	m := map[string]CronRun{}
	if b, err := os.ReadFile(d.Store.Path("apps", app, "cron.json")); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (d *Daemon) saveCronRun(app, command string, r CronRun) {
	cronStateMu.Lock()
	defer cronStateMu.Unlock()
	m := d.cronState(app)
	m[command] = r
	b, _ := json.MarshalIndent(m, "", "  ")
	state.WriteFile(d.Store.Path("apps", app, "cron.json"), append(b, '\n'), 0o600)
}

// runCron runs job idx of rel, logging to cron.log (and to out, for
// cron:run). A job still running from its last start is skipped.
func (d *Daemon) runCron(ctx context.Context, app string, rel *state.Release, idx int, trigger string, out io.Writer) error {
	key := fmt.Sprintf("%s/%d", app, idx)
	d.mu.Lock()
	busy := d.cronRunning[key]
	if !busy {
		d.cronRunning[key] = true
	}
	d.mu.Unlock()
	job := rel.Cron[idx-1]
	if busy {
		msg := fmt.Sprintf("cron %d (`%s`) is still running from its last start; skipped this one", idx, job.Command)
		if trigger == "schedule" {
			d.event(state.Event{Kind: "cron.skipped", App: app, Message: msg})
		}
		return fmt.Errorf("%s", msg)
	}
	defer func() {
		d.mu.Lock()
		delete(d.cronRunning, key)
		d.mu.Unlock()
	}()

	lw := NewRotatingWriter(d.Store.Path("apps", app, "logs", "cron.log"), 10<<20, 3)
	defer lw.Close()
	pw := &prefixWriter{w: lw, prefix: func() string {
		return fmt.Sprintf("%s v%d cron.%d: ", time.Now().UTC().Format(time.RFC3339), rel.N, idx)
	}}
	var w io.Writer = pw
	if out != nil {
		w = io.MultiWriter(pw, out)
	}
	start := time.Now()
	fmt.Fprintf(pw, "-----> %s (%s, %s)\n", job.Command, job.Schedule, trigger)
	err := d.Deployer.RunJob(ctx, app, rel, idx, w)
	secs := time.Since(start).Seconds()
	run := CronRun{Started: start, Secs: secs, Trigger: trigger}
	prev := d.cronState(app)[job.Command]
	if err != nil {
		run.Error = err.Error()
		run.Failures = 1
		if prev.Error != "" {
			run.Failures = max(prev.Failures, 1) + 1
		}
	}
	d.saveCronRun(app, job.Command, run)
	// Only the first failure in a row is an event (and a notification), so
	// a job that runs every minute against a sensor that's gone away
	// doesn't send sixty an hour; the rest are in cron.log and berth cron.
	if err != nil {
		fmt.Fprintf(pw, "-----> failed after %.1fs: %v\n", secs, err)
		if run.Failures == 1 {
			d.event(state.Event{Kind: "cron.failed", App: app, Message: fmt.Sprintf("cron %d (`%s`) failed after %.0fs: %v (berth logs %s --cron; you'll hear again when it succeeds)", idx, job.Command, secs, err, app)})
		}
	} else {
		fmt.Fprintf(pw, "-----> finished in %.1fs\n", secs)
		if prev.Error != "" {
			runs := "1 failed run"
			if n := max(prev.Failures, 1); n > 1 {
				runs = fmt.Sprintf("%d failed runs", n)
			}
			d.event(state.Event{Kind: "cron.ok", App: app, Message: fmt.Sprintf("cron %d (`%s`) succeeded again after %s", idx, job.Command, runs)})
		}
	}
	pw.Flush()
	if err != nil {
		slog.Warn("cron job failed", "app", app, "job", idx, "err", err)
	}
	return err
}

// prefixWriter starts every line with prefix().
type prefixWriter struct {
	w      io.Writer
	prefix func() string
	buf    bytes.Buffer
	mu     sync.Mutex
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	for {
		i := bytes.IndexByte(p.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := p.buf.Next(i + 1)
		io.WriteString(p.w, p.prefix()+string(line))
	}
	return len(b), nil
}

// Flush writes a final line that had no newline.
func (p *prefixWriter) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf.Len() > 0 {
		io.WriteString(p.w, p.prefix()+p.buf.String()+"\n")
		p.buf.Reset()
	}
}

// ---- RPC ----

type CronParams struct {
	App   string `json:"app"`
	Index int    `json:"index,omitempty"`
}

type CronJobStatus struct {
	Index    int       `json:"index"`
	Schedule string    `json:"schedule"`
	Command  string    `json:"command"`
	Next     time.Time `json:"next,omitempty"`
	Running  bool      `json:"running,omitempty"`
	Last     *CronRun  `json:"last,omitempty"`
}

type CronList struct {
	App     string          `json:"app"`
	Release int             `json:"release"`
	Paused  string          `json:"paused,omitempty"` // why schedules don't run
	Jobs    []CronJobStatus `json:"jobs"`
}

func (d *Daemon) hCronList(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p CronParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	a, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	res := CronList{App: a.Name, Jobs: []CronJobStatus{}}
	rel := d.Store.CurrentRelease(a.Name)
	if rel == nil {
		return res, nil
	}
	res.Release = rel.N
	switch {
	case a.DevOf != "":
		res.Paused = fmt.Sprintf("dev versions run jobs only by hand: berth cron:run %s <n>", a.Name)
	case a.Stopped:
		res.Paused = "the app is stopped"
	case rel.Status != state.StatusRunning:
		res.Paused = "the app isn't running"
	}
	last := d.cronState(a.Name)
	now := time.Now()
	for i, j := range rel.Cron {
		js := CronJobStatus{Index: i + 1, Schedule: j.Schedule, Command: j.Command}
		if s, err := cron.Parse(j.Schedule); err == nil {
			js.Next = s.Next(now)
		}
		if r, ok := last[j.Command]; ok {
			js.Last = &r
		}
		d.mu.Lock()
		js.Running = d.cronRunning[fmt.Sprintf("%s/%d", a.Name, i+1)]
		d.mu.Unlock()
		res.Jobs = append(res.Jobs, js)
	}
	return res, nil
}

func (d *Daemon) hCronRun(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p CronParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	a, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	rel := d.Store.CurrentRelease(a.Name)
	if rel == nil {
		return nil, fmt.Errorf("%s has no release yet", a.Name)
	}
	if len(rel.Cron) == 0 {
		return nil, fmt.Errorf("v%d has no cron jobs: add a \"cron\" list to app.json (see the README)", rel.N)
	}
	if p.Index < 1 || p.Index > len(rel.Cron) {
		return nil, fmt.Errorf("pick a job from 1 to %d (berth cron %s lists them)", len(rel.Cron), a.Name)
	}
	start := time.Now()
	err = d.runCron(ctx, a.Name, rel, p.Index, "cli", out)
	res := map[string]any{"app": a.Name, "release": rel.N, "index": p.Index, "command": rel.Cron[p.Index-1].Command, "secs": time.Since(start).Seconds()}
	if err != nil {
		return nil, fmt.Errorf("cron %d failed: %s", p.Index, strings.TrimSpace(err.Error()))
	}
	return res, nil
}
