package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// ---- one container at a time (design.md, SQLite) ----
//
// An app with a SQLite database keeps it on a volume that only one VM can
// attach, because SQLite's locks don't reach from one VM into another.
// So for these apps a deploy, rollback or restart stops the running
// release first (takeOver) instead of starting the new one beside it; if
// the new one doesn't come up, the old one is started again (giveBack).
// Workers don't run, and cron jobs run inside the web container.

// HelperImage runs the small jobs on a stopped app's SQLite volume.
const HelperImage = "alpine:3"

// volumes makes sure an app's storage folders exist and returns every -v
// its containers get.
func volumes(app *state.App) ([]string, error) {
	for _, m := range app.Mounts {
		if err := os.MkdirAll(m.Source, 0o755); err != nil {
			return nil, err
		}
	}
	return app.Volumes(), nil
}

// takeOver stops app's running release so a new container can attach the
// SQLite volume. The release's record keeps its container name until the
// swap (so Caddy's SRV answer still names its port and requests wait and
// retry rather than getting a 503); it returns that release, or nil if
// nothing was running or the app has no SQLite. Call with the app locked.
func (d *Deployer) takeOver(ctx context.Context, app *state.App, out io.Writer) *state.Release {
	if app.SQLite == nil {
		return nil
	}
	prev := d.Store.CurrentRelease(app.Name)
	if prev == nil || prev.Container == "" {
		return nil
	}
	d.mu.Lock()
	held := d.held[app.Name] == prev.Container
	d.mu.Unlock()
	if held {
		return prev
	}
	say(out, "Stopping v%d first: the SQLite database is open in one container at a time", prev.N)
	d.RemoveWorkerContainers(ctx, app.Name, prev)
	d.RT.Stop(ctx, prev.Container, 10*time.Second)
	d.RT.Remove(ctx, prev.Container)
	d.mu.Lock()
	d.held[app.Name] = prev.Container
	d.mu.Unlock()
	return prev
}

// heldFor reports whether takeOver stopped container for app.
func (d *Deployer) heldFor(app, container string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return container != "" && d.held[app] == container
}

// released records that the release takeOver stopped has been replaced.
func (d *Deployer) released(app string, prev *state.Release) {
	d.mu.Lock()
	delete(d.held, app)
	d.mu.Unlock()
	if r, err := d.Store.Release(app, prev.N); err == nil && d.Store.Current(app) != prev.N {
		r.Status, r.Container = state.StatusStopped, ""
		d.Store.SaveRelease(app, r)
	}
}

// giveBack starts the release takeOver stopped again, after the one meant
// to replace it failed. Call with the app locked.
func (d *Deployer) giveBack(ctx context.Context, app *state.App, cfg *state.Config, env state.Env, out io.Writer) {
	d.mu.Lock()
	_, held := d.held[app.Name]
	delete(d.held, app.Name)
	d.mu.Unlock()
	if !held {
		return
	}
	prev := d.Store.CurrentRelease(app.Name)
	if prev == nil {
		return
	}
	say(out, "Starting v%d again", prev.N)
	if err := d.startCheckSwapSame(ctx, app, cfg, prev, env, out); err != nil {
		prev.Container, prev.Status = "", state.StatusCrashed
		d.Store.SaveRelease(app.Name, prev)
		if d.Router != nil {
			d.Router.Apply(ctx)
		}
		detail(out, "v%d didn't come back either: %v", prev.N, err)
		d.Event(state.Event{Kind: "crash", App: app.Name, Message: fmt.Sprintf("v%d didn't restart after a failed deploy: %v", prev.N, err)})
		return
	}
	detail(out, "v%d is serving traffic again", prev.N)
}

// Paused runs fn with app's container stopped, so fn can attach its SQLite
// volume, then starts the current release again (unless the app was
// stopped already). wasRunning tells fn whether it was.
func (d *Deployer) Paused(ctx context.Context, appName string, out io.Writer, fn func(ctx context.Context, wasRunning bool) error) error {
	unlock := d.Lock(appName)
	defer unlock()
	return d.PausedLocked(ctx, appName, out, fn)
}

// PausedLocked is Paused for a caller that holds the app's lock.
func (d *Deployer) PausedLocked(ctx context.Context, appName string, out io.Writer, fn func(ctx context.Context, wasRunning bool) error) error {
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	env, _ := d.Store.Env(appName)
	rel := d.Store.CurrentRelease(appName)
	running := rel != nil && rel.Container != ""
	if running {
		d.RemoveWorkerContainers(ctx, appName, rel)
		d.RT.Stop(ctx, rel.Container, 10*time.Second)
		d.RT.Remove(ctx, rel.Container)
	}
	ferr := fn(ctx, running)
	if running {
		if err := d.startCheckSwapSame(ctx, app, cfg, rel, env, out); err != nil {
			rel.Container, rel.Status = "", state.StatusCrashed
			d.Store.SaveRelease(appName, rel)
			if d.Router != nil {
				d.Router.Apply(ctx)
			}
			if ferr != nil {
				return fmt.Errorf("%w; and %s didn't start again: %v", ferr, appName, err)
			}
			return fmt.Errorf("%s didn't start again: %w", appName, err)
		}
	}
	return ferr
}

// SQLiteCopyOut writes a tar of the SQLite volume (the database and any
// -wal file) to w, from a helper container. The app must not be running.
func (d *Deployer) SQLiteCopyOut(ctx context.Context, volume string, w io.Writer) error {
	var stderr capBuf
	err := d.RT.RunWith(ctx, runtime.RunOpts{
		Image:   HelperImage,
		Remove:  true,
		Volumes: []string{volume + ":/data"},
		Args:    []string{"sh", "-c", `cd /data && ls ` + state.SQLiteFile + `* >/dev/null 2>&1 || exit 3; tar -cf - ` + state.SQLiteFile + `*`},
		Stderr:  &stderr,
	}, nil, w)
	if err != nil && stderr.String() != "" {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return err
}

// SQLiteCopyIn replaces the database on the SQLite volume with the file r
// reads, keeping the owner the app's files had. The app must not be
// running.
func (d *Deployer) SQLiteCopyIn(ctx context.Context, volume string, r io.Reader) error {
	var stderr capBuf
	script := `set -e; cd /data
own=$(stat -c %u:%g ` + state.SQLiteFile + ` 2>/dev/null || stat -c %u:%g .)
cat > .restore.tmp
rm -f ` + state.SQLiteFile + `-wal ` + state.SQLiteFile + `-shm ` + state.SQLiteFile + `-journal
mv .restore.tmp ` + state.SQLiteFile + `
chown "$own" ` + state.SQLiteFile + `
chmod 0666 ` + state.SQLiteFile
	err := d.RT.RunWith(ctx, runtime.RunOpts{
		Image:       HelperImage,
		Remove:      true,
		Interactive: true,
		Volumes:     []string{volume + ":/data"},
		Args:        []string{"sh", "-c", script},
		Stderr:      &stderr,
	}, r, io.Discard)
	if err != nil && stderr.String() != "" {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return err
}

// SQLitePrepare creates an app's SQLite volume if it doesn't exist and
// lets any user write to it: the app may not run as root, and a new
// volume's root is root's. Only this app's container ever attaches it.
func (d *Deployer) SQLitePrepare(ctx context.Context, volume string) error {
	if !d.RT.VolumeExists(ctx, volume) {
		if err := d.RT.VolumeCreate(ctx, volume); err != nil {
			return err
		}
	}
	return d.RT.RunWith(ctx, runtime.RunOpts{
		Image:   HelperImage,
		Remove:  true,
		Volumes: []string{volume + ":/data"},
		Args:    []string{"chmod", "0777", "/data"},
	}, nil, io.Discard)
}

// capBuf keeps the first 4 KB written to it.
type capBuf struct{ b []byte }

func (c *capBuf) Write(p []byte) (int, error) {
	if room := 4096 - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (c *capBuf) String() string { return string(c.b) }
