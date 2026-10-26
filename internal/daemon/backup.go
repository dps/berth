package daemon

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/state"
)

// excluded reports whether a path (relative to ~/.berth) stays out of
// backups: re-fetchable git mirrors, build dirs, logs, the socket, and live
// service data (dumps are the backup).
func excluded(rel string, d fs.DirEntry) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	switch {
	case rel == "berthd.sock", rel == "berthd.log", strings.HasPrefix(rel, "events.log"), rel == filepath.Join("backups", "offsite"):
		return true
	case len(parts) >= 3 && parts[0] == "apps" && (parts[2] == "repo.git" || parts[2] == "build" || parts[2] == "logs"):
		return true
	case len(parts) >= 2 && parts[0] == "data" && parts[1] == "pg":
		return true
	case d.Type()&fs.ModeSocket != 0:
		return true
	}
	return false
}

func (d *Daemon) hBackup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg := d.cfg()
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		path, err := d.Services.Backup(ctx, sv.Kind, sv.Name, cfg.PGBackupKeep)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "-----> Dumped %s:%s → %s\n", sv.Kind, sv.Name, filepath.Base(path))
	}
	if p.Path == "" {
		home, _ := os.UserHomeDir()
		p.Path = filepath.Join(home, "berth-backup-"+time.Now().Format("20060102-150405")+".tar.gz")
	}
	n, err := WriteArchive(d.Store.Root, p.Path)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(p.Path)
	fmt.Fprintf(out, "-----> Wrote %s (%d files, %s)\n", p.Path, n, deploy.HumanBytes(fi.Size()))
	fmt.Fprintf(out, "       Restore on any Mac with `berth init` then `berth restore %s`\n", filepath.Base(p.Path))
	return map[string]string{"archive": p.Path}, nil
}

// WriteArchive tars root (minus exclusions) into a gzip file at dest.
func WriteArchive(root, dest string) (int, error) {
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	count, err := writeArchive(f, root)
	if err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		return 0, err
	}
	return count, os.Rename(tmp, dest)
}

// writeArchive writes root (minus exclusions) to w as a .tar.gz.
func writeArchive(w io.Writer, root string) (int, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	count := 0
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if excluded(rel, de) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			if err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	return count, err
}

// extractArchive unpacks into root, refusing paths that escape it. The
// local config.json is merged rather than replaced: machine-specific
// settings (hostname, gateway, launchd) stay those of this Mac.
func extractArchive(archive, root string) error { return extractFiltered(archive, root, nil) }

// SeedFromArchive puts a backup's secrets and settings (config.json, merged
// with this Mac's machine-specific values) in place before `berth init
// --restore` sets up Cloudflare and GitHub, so init reuses the old Mac's
// domain, tunnel and logins instead of asking for new ones. The rest comes
// back with `berth restore` once berthd is running.
func SeedFromArchive(archive string, store *state.Store) error {
	err := extractFiltered(archive, store.Root, func(name string) bool {
		return name == "config.json" || strings.HasPrefix(name, "secrets"+string(filepath.Separator))
	})
	if err != nil {
		return err
	}
	return mergeRestoredConfig(store)
}

func extractFiltered(archive, root string, keep func(name string) bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("archive entry %q escapes the berth directory", hdr.Name)
		}
		if keep != nil && !keep(name) {
			continue
		}
		target := filepath.Join(root, name)
		if name == "config.json" {
			target = filepath.Join(root, ".config.restored.json")
		}
		switch hdr.Typeflag {
		// Modes from the archive are ignored: everything under ~/.berth is
		// private to the user, whatever the archive says.
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o700)
			w, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, tr)
			w.Close()
			if err != nil {
				return err
			}
		}
	}
}

func mergeRestoredConfig(store *state.Store) error {
	restoredPath := store.Path(".config.restored.json")
	b, err := os.ReadFile(restoredPath)
	if err != nil {
		return nil
	}
	defer os.Remove(restoredPath)
	local, err := store.Config()
	if err != nil {
		return err
	}
	restored := state.DefaultConfig()
	if err := json.Unmarshal(b, restored); err != nil {
		return err
	}
	restored.Hostname = local.Hostname
	restored.Gateway = local.Gateway
	restored.Launchd = local.Launchd
	restored.Network = local.Network
	restored.CaddyAdmin, restored.CaddyHTTP, restored.CaddyHTTPS = local.CaddyAdmin, local.CaddyHTTP, local.CaddyHTTPS
	restored.WebhookAddr, restored.TunnelMetrics = local.WebhookAddr, local.TunnelMetrics
	return store.SaveConfig(restored)
}

// hRestore recreates apps, services and links from a `berth backup`
// archive: services get fresh volumes and their latest dump, apps are
// redeployed from their repos.
func (d *Daemon) hRestore(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p struct {
		Archive string `json:"archive"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.Archive); err != nil {
		return nil, err
	}
	if apps, _ := d.Store.Apps(); len(apps) > 0 {
		return nil, fmt.Errorf("restore expects a fresh install, but %d app(s) exist; destroy them first", len(apps))
	}
	d.restoring.Store(true)
	defer d.restoring.Store(false)
	fmt.Fprintf(out, "-----> Unpacking %s\n", filepath.Base(p.Archive))
	if err := extractArchive(p.Archive, d.Store.Root); err != nil {
		return nil, err
	}
	if err := mergeRestoredConfig(d.Store); err != nil {
		return nil, err
	}
	cfg := d.cfg()
	if gw, err := d.RT.EnsureNetwork(ctx, cfg.Network); err == nil && gw != cfg.Gateway {
		cfg.Gateway = gw
		d.Store.SaveConfig(cfg)
	}
	cs, _ := d.RT.List(ctx)
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		fmt.Fprintf(out, "-----> Recreating %s:%s\n", sv.Kind, sv.Name)
		fresh := !d.RT.VolumeExists(ctx, sv.Volume)
		if fresh {
			if err := d.RT.VolumeCreate(ctx, sv.Volume); err != nil {
				return nil, err
			}
		}
		if _, err := d.Services.EnsureRunning(ctx, sv, cs); err != nil {
			return nil, err
		}
		if err := d.Services.RelinkAll(ctx, sv); err != nil {
			return nil, err
		}
		if dump := d.Services.LatestBackup(sv.Kind, sv.Name); dump != "" && fresh {
			fmt.Fprintf(out, "       loading %s\n", filepath.Base(dump))
			if err := d.Services.Restore(ctx, sv.Kind, sv.Name, dump); err != nil {
				return nil, err
			}
		}
	}
	apps, _ := d.Store.Apps()
	var failed []string
	for _, a := range apps {
		// A preview's database isn't in the dumps and its code isn't in the
		// backup; it comes back from its pull request below.
		if a.Preview > 0 {
			d.destroyApp(ctx, a, false, io.Discard)
			continue
		}
		// Redeploy the commit that was live, not whatever the branch has
		// moved on to since.
		liveSHA := ""
		if cur := d.Store.CurrentRelease(a.Name); cur != nil {
			liveSHA = cur.SHA
		}
		// Releases on the old Mac reference images and containers that do
		// not exist here.
		rs, _ := d.Store.Releases(a.Name)
		for _, r := range rs {
			r.Container, r.Workers = "", nil
			if r.Status == state.StatusRunning || r.Status == state.StatusStarting {
				r.Status = state.StatusStopped
			}
			r.Pruned = true
			d.Store.SaveRelease(a.Name, r)
		}
		os.Remove(d.Store.Path("apps", a.Name, "current"))
		d.installHook(a)
		switch {
		case a.DevOf != "":
			fmt.Fprintf(out, "-----> %s is a dev version; the next `berth dev:push` from %s's repo brings it back\n", a.Name, a.DevOf)
			continue
		case a.Repo == "":
			fmt.Fprintf(out, "-----> %s has no repo; push to it to deploy\n", a.Name)
			continue
		case a.Stopped:
			continue
		}
		req := deployReq(a.Name, out)
		req.Ref = liveSHA
		rel, err := d.Deployer.Deploy(ctx, req)
		if err != nil && rel == nil && liveSHA != "" {
			// The commit is gone from the repo (a force-push): its branch, then.
			fmt.Fprintf(out, "       %s isn't in %s any more; deploying %s instead\n", liveSHA[:min(7, len(liveSHA))], a.Repo, a.Branch)
			_, err = d.Deployer.Deploy(ctx, deployReq(a.Name, out))
		}
		if err != nil {
			failed = append(failed, a.Name)
			fmt.Fprintf(out, "       %s failed to deploy: %v\n", a.Name, err)
		}
	}
	d.Apply(ctx)
	d.ApplyTunnel(ctx, true)
	for _, a := range apps {
		if a.PreviewsOn {
			go d.syncPreviews(d.ctx, a.Name)
		}
	}
	if d.Store.AIConfigured() {
		fmt.Fprintln(out, "-----> AI settings are back. If apps use local models, run `berth ai:setup` to install Ollama and download them")
	}
	if cfg.Hostname != "" {
		fmt.Fprintf(out, "-----> LAN addresses now end in .%s.local\n", cfg.Hostname)
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("restored, but these apps failed to deploy: %s", strings.Join(failed, ", "))
	}
	fmt.Fprintf(out, "-----> Restored %d app(s) and %d service(s)\n", len(apps), len(svs))
	return map[string]int{"apps": len(apps), "services": len(svs)}, nil
}

func deployReq(app string, out io.Writer) deploy.Request {
	return deploy.Request{App: app, Trigger: "restore", Out: out}
}
