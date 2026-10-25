package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/state"
)

// releaseBackup dumps each Postgres database app is linked to, just before
// its release command runs (design.md, Release commands).
func (d *Daemon) releaseBackup(ctx context.Context, app *state.App, rel *state.Release, out io.Writer) (map[string]string, error) {
	keep := d.cfg().PGBackupKeep
	backups := map[string]string{}
	for _, link := range app.Links {
		name, ok := strings.CutPrefix(link, "pg:")
		if !ok {
			continue
		}
		start := time.Now()
		path, err := d.Services.ReleaseBackup(ctx, name, app.Name, rel.N, keep)
		if err != nil {
			return nil, err
		}
		size := ""
		if fi, err := os.Stat(path); err == nil {
			size = ", " + deploy.HumanBytes(fi.Size())
		}
		fmt.Fprintf(out, "-----> Backed up pg:%s before the release command (%.1fs%s)\n", name, time.Since(start).Seconds(), size)
		backups[link] = path
	}
	return backups, nil
}

// restoreData replaces each database with its pre-release dump.
func (d *Daemon) restoreData(ctx context.Context, app *state.App, backups map[string]string, out io.Writer) error {
	for link, file := range backups {
		name, ok := strings.CutPrefix(link, "pg:")
		if !ok {
			continue
		}
		safety, err := d.Services.Replace(ctx, name, file)
		if safety != "" {
			fmt.Fprintf(out, "       pg:%s as it was a moment ago is saved in %s\n", name, safety)
		}
		if err != nil {
			return fmt.Errorf("pg:%s: %w", name, err)
		}
		fmt.Fprintf(out, "-----> Restored pg:%s from %s\n", name, file)
	}
	return nil
}

// DataPlan is what `berth rollback --with-data` would restore, for its
// confirmation prompt.
type DataPlan struct {
	Target   int       `json:"target"`
	From     int       `json:"from"` // the release whose pre-release backups are used
	TakenAt  time.Time `json:"taken_at"`
	Services []string  `json:"services"`
}

func (d *Daemon) hRollbackPlan(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AppParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	target, err := d.Deployer.RollbackTarget(p.App, p.N)
	if err != nil {
		return nil, err
	}
	from, err := d.Deployer.DataFor(p.App, target.N)
	if err != nil {
		return nil, err
	}
	plan := &DataPlan{Target: target.N, From: from.N, TakenAt: from.CreatedAt}
	for svc, f := range from.Backups {
		plan.Services = append(plan.Services, svc)
		if fi, err := os.Stat(f); err == nil {
			plan.TakenAt = fi.ModTime()
		}
	}
	sort.Strings(plan.Services)
	return plan, nil
}
