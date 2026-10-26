package deploy

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/dps/berth/internal/state"
)

// RunJob runs one of rel's cron jobs to completion in a fresh container
// from the release's image and config, writing its output to w. Unlike
// deploys it doesn't take the app's lock: a long job mustn't hold up
// deploys, and it keeps running on the image it started with.
func (d *Deployer) RunJob(ctx context.Context, appName string, rel *state.Release, idx int, w io.Writer) error {
	if idx < 1 || idx > len(rel.Cron) {
		return fmt.Errorf("v%d has no cron job %d", rel.N, idx)
	}
	app, err := d.Store.App(appName)
	if err != nil {
		return err
	}
	cfg, err := d.Store.Config()
	if err != nil {
		return err
	}
	env, _ := d.Store.Env(appName)
	opts, err := d.oneOffOpts(app, cfg, rel, env, "cron")
	if err != nil {
		return err
	}
	opts.Name = fmt.Sprintf("%s-cron%d-%s", state.ContainerName(app.Name, rel.N), idx, strconv.FormatInt(time.Now().UnixMilli()%1_000_000, 36))
	opts.Entrypoint, opts.Args = "/bin/sh", []string{"-c", rel.Cron[idx-1].Command}
	opts.Remove, opts.Stderr = true, w
	timeout := time.Duration(env.IntSetting(state.KeyCronTimeout, 3600)) * time.Second
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = d.RT.RunWith(jctx, opts, nil, w)
	if err != nil {
		d.RT.Remove(context.Background(), opts.Name)
		if jctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timed out after %s (%s raises it)", timeout, state.KeyCronTimeout)
		}
		if m := exitStatusRe.FindStringSubmatch(err.Error()); m != nil {
			return fmt.Errorf("exited with status %s", m[1])
		}
	}
	return err
}

var exitStatusRe = regexp.MustCompile(`exit status (\d+)`)
