package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dps/berth/internal/detect"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// ---- workers: the Procfile's other process types (design.md, Workers and cron) ----

// MaxWorkers caps ps:scale per process type.
const MaxWorkers = 10

// readProcesses records the build's worker process types and cron jobs on
// rel. A malformed app.json cron list fails the deploy rather than
// silently dropping a schedule.
func readProcesses(dir string, rel *state.Release, out io.Writer) error {
	procs, bad := detect.ProcfileWorkers(dir)
	if len(bad) > 0 {
		say(out, "Ignoring Procfile process types %s: names are lowercase letters and digits, up to 20", strings.Join(bad, ", "))
	}
	rel.Processes = procs
	jobs, err := detect.AppJSONCron(dir)
	if err != nil {
		return err
	}
	rel.Cron = nil
	for _, j := range jobs {
		rel.Cron = append(rel.Cron, state.CronJob{Command: j.Command, Schedule: j.Schedule})
	}
	return nil
}

// oneOffOpts is how release commands, workers and cron jobs run: the
// release's image, config and mounts, no published port.
func (d *Deployer) oneOffOpts(app *state.App, cfg *state.Config, rel *state.Release, env state.Env, role string) (runtime.RunOpts, error) {
	envFile := d.Store.Path("apps", app.Name, "releases", fmt.Sprintf("%d.env", rel.N))
	file, extra := state.Env(rel.Env).ContainerEnvFile()
	if err := state.WriteFile(envFile, []byte(file), 0o600); err != nil {
		return runtime.RunOpts{}, err
	}
	var vols []string
	for _, m := range app.Mounts {
		if err := os.MkdirAll(m.Source, 0o755); err != nil {
			return runtime.RunOpts{}, err
		}
		vols = append(vols, m.Source+":"+m.Path)
	}
	return runtime.RunOpts{
		Image:   rel.Image,
		Network: cfg.Network,
		EnvFile: envFile,
		Env:     extra,
		CPUs:    env.StrSetting(state.KeyCPUs, cfg.DefaultCPUs),
		Memory:  env.StrSetting(state.KeyMemory, cfg.DefaultMemory),
		Volumes: vols,
		Labels:  map[string]string{"dev.berth.app": app.Name, "dev.berth.release": strconv.Itoa(rel.N), "dev.berth.role": role},
		Init:    true,
	}, nil
}

func (d *Deployer) runWorker(ctx context.Context, app *state.App, cfg *state.Config, rel *state.Release, env state.Env, typ string, idx int) (state.Worker, error) {
	w := state.Worker{Type: typ, Index: idx}
	opts, err := d.oneOffOpts(app, cfg, rel, env, "worker")
	if err != nil {
		return w, err
	}
	w.Container = fmt.Sprintf("%s-%s%d-%s", state.ContainerName(app.Name, rel.N), typ, idx, strconv.FormatInt(time.Now().UnixMilli()%1_000_000, 36))
	opts.Name = w.Container
	opts.Labels["dev.berth.process"] = w.Name()
	opts.Entrypoint, opts.Args = "/bin/sh", []string{"-c", rel.Processes[typ]}
	opts.Detach = true
	d.RT.Remove(ctx, w.Container)
	return w, d.RT.Run(ctx, opts)
}

// startWorkers runs rel's worker processes at the app's scale. The web
// process is already live, so a worker that won't start is reported but
// doesn't fail the deploy; the supervisor keeps restarting it.
func (d *Deployer) startWorkers(ctx context.Context, app *state.App, cfg *state.Config, rel *state.Release, env state.Env, out io.Writer) {
	types := make([]string, 0, len(rel.Processes))
	for t := range rel.Processes {
		types = append(types, t)
	}
	sort.Strings(types)
	var started []string
	for _, t := range types {
		for i := 1; i <= app.WorkerCount(t); i++ {
			w, err := d.runWorker(ctx, app, cfg, rel, env, t, i)
			if err != nil {
				detail(out, "warning: %s didn't start: %v", w.Name(), err)
				continue
			}
			rel.Workers = append(rel.Workers, w)
			started = append(started, w.Name())
		}
	}
	if len(started) == 0 {
		return
	}
	d.Store.SaveRelease(app.Name, rel)
	say(out, "Started %s", strings.Join(started, ", "))
	// A worker that exits straight away has a config or code problem worth
	// showing now rather than only in the logs.
	time.Sleep(workerSettle)
	cs, err := d.RT.List(ctx)
	if err != nil {
		return
	}
	for _, w := range rel.Workers {
		if c, ok := runtime.Find(cs, w.Container); !ok || !c.Running() {
			detail(out, "warning: %s exited right after starting; Berth will keep restarting it. Its last lines:", w.Name())
			if logs := d.lastLogs(ctx, w.Container, 20); logs != "" {
				fmt.Fprint(out, indentString(logs))
			}
		}
	}
}

// workerSettle is how long startWorkers waits before checking that
// workers are still running.
var workerSettle = 3 * time.Second

// stopWorkers stops and removes rel's worker containers.
func (d *Deployer) stopWorkers(ctx context.Context, appName string, rel *state.Release, out io.Writer) {
	if len(rel.Workers) == 0 {
		return
	}
	for _, w := range rel.Workers {
		d.RT.Stop(ctx, w.Container, 10*time.Second)
		d.RT.Remove(ctx, w.Container)
	}
	if out != nil && rel.N != 0 {
		say(out, "Stopped v%d's workers", rel.N)
	}
	rel.Workers = nil
	d.Store.SaveRelease(appName, rel)
}

// RecoverWorker restarts one worker of the current release (the
// supervisor's crash restart).
func (d *Deployer) RecoverWorker(ctx context.Context, appName, name string) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil || app.Stopped {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	rel := d.Store.CurrentRelease(appName)
	if rel == nil {
		return nil
	}
	env, _ := d.Store.Env(appName)
	for i, w := range rel.Workers {
		if w.Name() != name {
			continue
		}
		d.RT.Remove(ctx, w.Container)
		nw, err := d.runWorker(ctx, app, cfg, rel, env, w.Type, w.Index)
		if err != nil {
			return err
		}
		rel.Workers[i] = nw
		return d.Store.SaveRelease(appName, rel)
	}
	return nil
}

// Scale sets how many containers run for a process type of the current
// release, starting or stopping workers to match.
func (d *Deployer) Scale(ctx context.Context, appName, typ string, n int, out io.Writer) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	if n < 0 || n > MaxWorkers {
		return fmt.Errorf("scale is 0 to %d", MaxWorkers)
	}
	rel := d.Store.CurrentRelease(appName)
	if rel == nil {
		return ErrNothingToDeploy
	}
	if _, ok := rel.Processes[typ]; !ok {
		var have []string
		for t := range rel.Processes {
			have = append(have, t)
		}
		sort.Strings(have)
		if len(have) == 0 {
			return fmt.Errorf("v%d has no worker process types: add lines like `worker: node worker.js` to the Procfile", rel.N)
		}
		return fmt.Errorf("v%d has no %q process type (it has %s)", rel.N, typ, strings.Join(have, ", "))
	}
	if app.Scale == nil {
		app.Scale = map[string]int{}
	}
	app.Scale[typ] = n
	if err := d.Store.SaveApp(app); err != nil {
		return err
	}
	if app.Stopped || rel.Container == "" {
		say(out, "%s will run %d %s when it starts", appName, n, typ)
		return nil
	}
	env, _ := d.Store.Env(appName)
	var keep []state.Worker
	have := 0
	for _, w := range rel.Workers {
		if w.Type == typ && w.Index > n {
			d.RT.Stop(ctx, w.Container, 10*time.Second)
			d.RT.Remove(ctx, w.Container)
			say(out, "Stopped %s", w.Name())
			continue
		}
		if w.Type == typ {
			have = max(have, w.Index)
		}
		keep = append(keep, w)
	}
	rel.Workers = keep
	for i := have + 1; i <= n; i++ {
		w, err := d.runWorker(ctx, app, cfg, rel, env, typ, i)
		if err != nil {
			d.Store.SaveRelease(appName, rel)
			return fmt.Errorf("%s didn't start: %w", w.Name(), err)
		}
		rel.Workers = append(rel.Workers, w)
		say(out, "Started %s", w.Name())
	}
	return d.Store.SaveRelease(appName, rel)
}
