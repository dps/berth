package deploy

import (
	"context"
	"fmt"
	"io"
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

// oneOffOpts is how release commands and cron jobs run: the
// release's image, config and mounts, no published port.
func (d *Deployer) oneOffOpts(app *state.App, cfg *state.Config, rel *state.Release, env state.Env, role string) (runtime.RunOpts, error) {
	envFile := d.Store.Path("apps", app.Name, "releases", fmt.Sprintf("%d.env", rel.N))
	file, extra := state.Env(rel.Env).ContainerEnvFile()
	if err := state.WriteFile(envFile, []byte(file), 0o600); err != nil {
		return runtime.RunOpts{}, err
	}
	vols, err := volumes(app)
	if err != nil {
		return runtime.RunOpts{}, err
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

// workers puts the app's Procfile workers in its live web container, or
// stops them when it has none (berthd's Workers hook, design.md, Workers
// and cron). out gets what started, and any worker that exited straight
// away.
func (d *Deployer) workers(ctx context.Context, appName string, out io.Writer) {
	if d.Workers != nil {
		d.Workers(ctx, appName, out)
	}
}

// RemoveWorkerContainers stops and removes the worker containers an older
// Berth ran for rel (workers now run inside the web container).
func (d *Deployer) RemoveWorkerContainers(ctx context.Context, appName string, rel *state.Release) {
	if len(rel.Workers) == 0 {
		return
	}
	for _, w := range rel.Workers {
		d.RT.Stop(ctx, w.Container, 10*time.Second)
		d.RT.Remove(ctx, w.Container)
	}
	rel.Workers = nil
	d.Store.SaveRelease(appName, rel)
}

// Scale sets how many of a process type of the current release run,
// starting or stopping workers to match.
func (d *Deployer) Scale(ctx context.Context, appName, typ string, n int, out io.Writer) error {
	unlock := d.Lock(appName)
	defer unlock()
	app, err := d.Store.App(appName)
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
	d.workers(ctx, appName, out)
	return nil
}
