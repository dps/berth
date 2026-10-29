package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/offsite"
	"github.com/dps/berth/internal/state"
)

// ---- off-Mac backups (design.md, Off-Mac backups) ----
//
// Nightly, after the Postgres dumps, the same archive `berth backup` makes
// is encrypted on the Mac and uploaded to an S3-compatible bucket; the
// newest 14 are kept. The key never leaves the Mac except when the owner
// asks for it, so the bucket only holds ciphertext.

var offsiteMu sync.Mutex

// offsiteRun uploads one backup. dump says whether to take fresh database
// dumps first (the nightly run follows the nightly dumps, so it doesn't).
func (d *Daemon) offsiteRun(ctx context.Context, dump bool, trigger string, out io.Writer) (res *offsite.Status, err error) {
	if !offsiteMu.TryLock() {
		return nil, fmt.Errorf("an off-Mac backup is already running")
	}
	defer offsiteMu.Unlock()
	root := d.Store.Root
	cfg, err := offsite.LoadConfig(root)
	if err != nil {
		return nil, err
	}
	key, err := offsite.LoadKey(root)
	if err != nil {
		return nil, fmt.Errorf("the backup key is missing (%s): %w", offsite.KeyPath(root), err)
	}
	st := offsite.LoadStatus(root)
	st.LastTry = time.Now()
	start := time.Now()
	defer func() {
		st.LastSecs = time.Since(start).Seconds()
		if err != nil {
			st.LastError = err.Error()
			d.event(state.Event{Kind: "offsite.failed", Message: fmt.Sprintf("off-Mac backup to %s failed: %v", cfg.Bucket, err)})
		}
		offsite.SaveStatus(root, st)
		res = &st
	}()

	if dump {
		svs, _ := d.Store.Services("")
		for _, sv := range svs {
			path, err := d.Services.Backup(ctx, sv.Kind, sv.Name, d.cfg().PGBackupKeep)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(out, "-----> Dumped %s:%s → %s\n", sv.Kind, sv.Name, filepath.Base(path))
		}
		d.backupAllSQLite(ctx, false, out)
	}
	dir := d.Store.Path("backups", "offsite")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := cfg.ObjectName(start)
	enc := filepath.Join(dir, filepath.Base(name))
	defer os.Remove(enc)
	// The archive streams straight into the encryption: no plaintext copy
	// is written.
	f, err := os.OpenFile(enc, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	var files int
	go func() {
		n, err := writeArchive(pw, root)
		files = n
		pw.CloseWithError(err)
	}()
	err = offsite.Encrypt(f, pr, key)
	pr.CloseWithError(err)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("making the archive: %w", err)
	}
	fi, err := os.Stat(enc)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> Encrypted %d files (%s); uploading to bucket %s as %s\n", files, deploy.HumanBytes(fi.Size()), cfg.Bucket, name)
	if err := cfg.Client().PutFile(ctx, name, enc); err != nil {
		return nil, fmt.Errorf("uploading: %w", err)
	}
	// Check it arrived whole before counting it.
	objs, err := cfg.Backups(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking the upload: %w", err)
	}
	found := false
	for _, o := range objs {
		found = found || (o.Key == name && o.Size == fi.Size())
	}
	if !found {
		return nil, fmt.Errorf("the upload didn't arrive whole in %s", cfg.Bucket)
	}
	gone, err := cfg.Prune(ctx)
	if err != nil {
		fmt.Fprintf(out, "       warning: removing old backups: %v\n", err)
	}
	if len(gone) > 0 {
		fmt.Fprintf(out, "-----> Removed %d old backup(s); keeping the newest %d\n", len(gone), cfg.Keep)
	}
	fmt.Fprintf(out, "-----> Uploaded in %.0fs\n", time.Since(start).Seconds())
	if st.LastError != "" {
		d.event(state.Event{Kind: "offsite", Message: "off-Mac backups are working again"})
	}
	st.LastOK, st.LastKey, st.LastSize, st.LastError = time.Now(), name, fi.Size(), ""
	if trigger != "nightly" {
		d.event(state.Event{Kind: "offsite", Message: fmt.Sprintf("backup uploaded to %s/%s (%s)", cfg.Bucket, name, deploy.HumanBytes(fi.Size()))})
	}
	return &st, nil
}

// offsiteWarning is the status warning when off-Mac backups are set up but
// haven't worked lately.
func (d *Daemon) offsiteWarning() string {
	if _, err := offsite.LoadConfig(d.Store.Root); err != nil {
		return ""
	}
	st := offsite.LoadStatus(d.Store.Root)
	if st.LastTry.IsZero() || time.Since(st.LastOK) < 48*time.Hour {
		return ""
	}
	if st.LastOK.IsZero() {
		return fmt.Sprintf("no off-Mac backup has worked yet: %s (berth offsite:run)", st.LastError)
	}
	return fmt.Sprintf("the last off-Mac backup that worked was %s ago: %s (berth offsite:run)", time.Since(st.LastOK).Round(time.Hour), st.LastError)
}

// ---- RPC ----

type OffsiteParams struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key_id,omitempty"`
	SecretKey string `json:"secret_access_key,omitempty"`
	Keep      int    `json:"keep,omitempty"`
	Key       string `json:"key,omitempty"` // an existing backup key (berthkey_…)
	Object    string `json:"object,omitempty"`
	Dest      string `json:"dest,omitempty"`
}

type OffsiteStatus struct {
	SetUp    bool             `json:"set_up"`
	Endpoint string           `json:"endpoint,omitempty"`
	Bucket   string           `json:"bucket,omitempty"`
	Prefix   string           `json:"prefix,omitempty"`
	Keep     int              `json:"keep,omitempty"`
	KeyID    string           `json:"access_key_id,omitempty"` // masked
	Last     offsite.Status   `json:"last"`
	Backups  []offsite.Object `json:"backups"`
	ListErr  string           `json:"list_error,omitempty"`
	Recovery string           `json:"recovery_code,omitempty"` // from setup: bucket, credentials and key
	NewKey   bool             `json:"new_key,omitempty"`       // setup made the encryption key
}

func (d *Daemon) hOffsiteSetup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p OffsiteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	root := d.Store.Root
	cfg := &offsite.Config{Endpoint: strings.TrimSpace(p.Endpoint), Region: strings.TrimSpace(p.Region), Bucket: strings.TrimSpace(p.Bucket),
		Prefix: strings.TrimSpace(p.Prefix), AccessKey: strings.TrimSpace(p.AccessKey), SecretKey: strings.TrimSpace(p.SecretKey), Keep: p.Keep}
	if cfg.Prefix == "" {
		host := d.cfg().Hostname
		if host == "" {
			host, _ = os.Hostname()
		}
		cfg.Prefix = "berth/" + strings.ToLower(strings.TrimSuffix(host, ".local")) + "/"
	}
	if !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	if cfg.Keep <= 0 {
		cfg.Keep = offsite.DefaultKeep
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Prove the credentials can write, read back, list and delete.
	c := cfg.Client()
	probe := cfg.Prefix + ".berth-check"
	tmp, err := os.CreateTemp("", "berth-check")
	if err != nil {
		return nil, err
	}
	tmp.WriteString("berth\n")
	tmp.Close()
	defer os.Remove(tmp.Name())
	fmt.Fprintf(out, "-----> Checking %s/%s at %s\n", cfg.Bucket, cfg.Prefix, cfg.Endpoint)
	if err := c.PutFile(ctx, probe, tmp.Name()); err != nil {
		return nil, fmt.Errorf("writing a test file: %w", err)
	}
	if rc, err := c.Get(ctx, probe); err != nil {
		return nil, fmt.Errorf("reading the test file back: %w", err)
	} else {
		rc.Close()
	}
	if _, err := cfg.Backups(ctx); err != nil {
		return nil, fmt.Errorf("listing the bucket: %w", err)
	}
	if err := c.Delete(ctx, probe); err != nil {
		return nil, fmt.Errorf("deleting the test file (Berth removes old backups, so it needs delete too): %w", err)
	}
	res := OffsiteStatus{}
	switch existing, kerr := offsite.LoadKey(root); {
	case p.Key != "":
		k, err := offsite.ParseAnyKey(p.Key)
		if err != nil {
			return nil, err
		}
		if kerr == nil && string(existing) != string(k) {
			return nil, fmt.Errorf("this Mac already has a different backup key; backups made with it would become unreadable. Move %s aside first if you mean it", offsite.KeyPath(root))
		}
		if err := offsite.SaveKey(root, k); err != nil {
			return nil, err
		}
	case kerr == nil:
		// Keep the key: earlier backups need it.
	default:
		if err := offsite.SaveKey(root, offsite.NewKey()); err != nil {
			return nil, err
		}
		res.NewKey = true
	}
	if err := offsite.SaveConfig(root, cfg); err != nil {
		return nil, err
	}
	key, err := offsite.LoadKey(root)
	if err != nil {
		return nil, err
	}
	res.Recovery = offsite.RecoveryCode(cfg, key)
	d.event(state.Event{Kind: "offsite", Message: fmt.Sprintf("off-Mac backups go to %s/%s", cfg.Bucket, cfg.Prefix)})
	res.SetUp, res.Endpoint, res.Bucket, res.Prefix, res.Keep = true, cfg.Endpoint, cfg.Bucket, cfg.Prefix, cfg.Keep
	return res, nil
}

func (d *Daemon) hOffsiteRun(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	st, err := d.offsiteRun(ctx, true, "cli", out)
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (d *Daemon) hOffsiteStatus(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	cfg, err := offsite.LoadConfig(d.Store.Root)
	if errors.Is(err, offsite.ErrNotSetUp) {
		return OffsiteStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	res := OffsiteStatus{SetUp: true, Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Prefix: cfg.Prefix, Keep: cfg.Keep,
		KeyID: state.Mask(cfg.AccessKey), Last: offsite.LoadStatus(d.Store.Root), Backups: []offsite.Object{}}
	if objs, err := cfg.Backups(ctx); err != nil {
		res.ListErr = err.Error()
	} else {
		res.Backups = objs
	}
	return res, nil
}

func (d *Daemon) hOffsiteKey(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	k, err := offsite.LoadKey(d.Store.Root)
	if err != nil {
		return nil, fmt.Errorf("this Mac has no backup key yet (berth offsite:setup makes one)")
	}
	res := map[string]string{"key": offsite.EncodeKey(k)}
	if cfg, err := offsite.LoadConfig(d.Store.Root); err == nil {
		res["recovery_code"] = offsite.RecoveryCode(cfg, k)
	}
	return res, nil
}

// hOffsiteGet downloads and decrypts a backup to a .tar.gz on the Mac,
// ready for `berth restore`.
func (d *Daemon) hOffsiteGet(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p OffsiteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg, err := offsite.LoadConfig(d.Store.Root)
	if err != nil {
		return nil, err
	}
	var key []byte
	if p.Key != "" {
		key, err = offsite.ParseAnyKey(p.Key)
	} else {
		key, err = offsite.LoadKey(d.Store.Root)
	}
	if err != nil {
		return nil, fmt.Errorf("no backup key: give your recovery code with --key berthrestore_…")
	}
	dest := p.Dest
	if dest == "" {
		home, _ := os.UserHomeDir()
		dest = filepath.Join(home, "berth-restore-"+time.Now().Format("20060102-150405")+".tar.gz")
	}
	fmt.Fprintf(out, "-----> Downloading and decrypting from %s\n", cfg.Bucket)
	obj, err := cfg.Fetch(ctx, key, p.Object, dest)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "-----> %s (%s, %s) → %s\n", obj.Key, deploy.HumanBytes(obj.Size), obj.Modified.Local().Format("2006-01-02 15:04"), dest)
	fmt.Fprintf(out, "       Restore it with `berth restore %s`\n", dest)
	return map[string]string{"archive": dest, "object": obj.Key}, nil
}

func (d *Daemon) hOffsiteOff(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	if err := os.Remove(offsite.ConfigPath(d.Store.Root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fmt.Fprintln(out, "-----> Off-Mac backups are off. The bucket and the backups in it are untouched, and this Mac keeps its backup key.")
	d.event(state.Event{Kind: "offsite", Message: "off-Mac backups turned off"})
	return map[string]bool{"set_up": false}, nil
}
