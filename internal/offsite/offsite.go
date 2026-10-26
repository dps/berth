package offsite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is where backups go: secrets/offsite.json (0600).
type Config struct {
	Endpoint  string `json:"endpoint"` // https://<account>.r2.cloudflarestorage.com
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"` // ends in /
	AccessKey string `json:"access_key_id"`
	SecretKey string `json:"secret_access_key"`
	Keep      int    `json:"keep"`
}

// DefaultKeep is how many backups stay in the bucket.
const DefaultKeep = 14

// Status is the last run, in offsite.json beside config.json (no secrets).
type Status struct {
	LastTry   time.Time `json:"last_try,omitempty"`
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastKey   string    `json:"last_key,omitempty"`
	LastSize  int64     `json:"last_size,omitempty"`
	LastSecs  float64   `json:"last_secs,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// Files under ~/.berth.
func ConfigPath(root string) string { return filepath.Join(root, "secrets", "offsite.json") }
func KeyPath(root string) string    { return filepath.Join(root, "secrets", "offsite.key") }
func StatusPath(root string) string { return filepath.Join(root, "offsite.json") }

// ErrNotSetUp means no bucket is configured.
var ErrNotSetUp = errors.New("off-Mac backups aren't set up (berth offsite:setup)")

func LoadConfig(root string) (*Config, error) {
	b, err := os.ReadFile(ConfigPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotSetUp
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(root), err)
	}
	if c.Keep <= 0 {
		c.Keep = DefaultKeep
	}
	return c, nil
}

func writeSecret(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func SaveConfig(root string, c *Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeSecret(ConfigPath(root), append(b, '\n'))
}

func LoadKey(root string) ([]byte, error) {
	b, err := os.ReadFile(KeyPath(root))
	if err != nil {
		return nil, err
	}
	return ParseKey(string(b))
}

func SaveKey(root string, key []byte) error {
	return writeSecret(KeyPath(root), []byte(EncodeKey(key)+"\n"))
}

func LoadStatus(root string) Status {
	var s Status
	if b, err := os.ReadFile(StatusPath(root)); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func SaveStatus(root string, s Status) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return writeSecret(StatusPath(root), append(b, '\n'))
}

// Validate checks a config before it's saved.
func (c *Config) Validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLocal(u.Hostname()))) {
		return fmt.Errorf("the endpoint must be an https:// URL, like https://<account id>.r2.cloudflarestorage.com")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("the endpoint is the service's address only; give the bucket separately")
	}
	if !bucketRe.MatchString(c.Bucket) {
		return fmt.Errorf("%q isn't a bucket name", c.Bucket)
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return fmt.Errorf("an access key ID and secret are needed")
	}
	if strings.HasPrefix(c.Prefix, "/") || strings.Contains(c.Prefix, "..") {
		return fmt.Errorf("the prefix is a folder inside the bucket, like berth/")
	}
	return nil
}

func isLocal(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Client returns an S3 client for c.
func (c *Config) Client() *S3 {
	region := c.Region
	if region == "" {
		region = "auto"
		if strings.HasSuffix(strings.TrimSuffix(c.Endpoint, "/"), "amazonaws.com") {
			region = "us-east-1"
		}
	}
	return &S3{Endpoint: c.Endpoint, Region: region, Bucket: c.Bucket, AccessKey: c.AccessKey, SecretKey: c.SecretKey}
}

// ObjectName is the key a backup taken at t goes to.
func (c *Config) ObjectName(t time.Time) string {
	return c.Prefix + "berth-" + t.UTC().Format("20060102-150405") + ".tar.gz.enc"
}

var backupRe = regexp.MustCompile(`berth-\d{8}-\d{6}\.tar\.gz\.enc$`)

// Backups lists the backups in the bucket, oldest first. Other objects
// under the prefix are left alone.
func (c *Config) Backups(ctx context.Context) ([]Object, error) {
	objs, err := c.Client().List(ctx, c.Prefix)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, o := range objs {
		if rest := strings.TrimPrefix(o.Key, c.Prefix); !strings.Contains(rest, "/") && backupRe.MatchString(rest) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Prune deletes all but the newest keep backups and returns the deleted keys.
func (c *Config) Prune(ctx context.Context) ([]string, error) {
	objs, err := c.Backups(ctx)
	if err != nil {
		return nil, err
	}
	var gone []string
	for len(objs) > c.Keep {
		if err := c.Client().Delete(ctx, objs[0].Key); err != nil {
			return gone, err
		}
		gone = append(gone, objs[0].Key)
		objs = objs[1:]
	}
	return gone, nil
}

// ---- recovery codes ----
//
// A recovery code is everything a new Mac needs to find and read the
// backups, the bucket's address and credentials and the encryption key, as
// one string to keep in a password manager: berthrestore_<base64 JSON>.

const recoveryPrefix = "berthrestore_"

type recovery struct {
	V int `json:"v"`
	Config
	Key string `json:"key"`
}

// RecoveryCode encodes c and key.
func RecoveryCode(c *Config, key []byte) string {
	b, _ := json.Marshal(recovery{V: 1, Config: *c, Key: EncodeKey(key)})
	return recoveryPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseRecoveryCode decodes a recovery code.
func ParseRecoveryCode(s string) (*Config, []byte, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), recoveryPrefix)
	if !ok {
		return nil, nil, errors.New("that isn't a Berth recovery code (berthrestore_…)")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, nil, errors.New("the recovery code is damaged: copy all of it")
	}
	var r recovery
	if err := json.Unmarshal(b, &r); err != nil || r.V != 1 {
		return nil, nil, errors.New("the recovery code is damaged: copy all of it")
	}
	key, err := ParseKey(r.Key)
	if err != nil {
		return nil, nil, err
	}
	c := r.Config
	if c.Keep <= 0 {
		c.Keep = DefaultKeep
	}
	return &c, key, c.Validate()
}

// ParseAnyKey reads a key, or the key inside a recovery code.
func ParseAnyKey(s string) ([]byte, error) {
	if strings.HasPrefix(strings.TrimSpace(s), recoveryPrefix) {
		_, k, err := ParseRecoveryCode(s)
		return k, err
	}
	return ParseKey(s)
}

// Fetch downloads backup obj (the newest when obj is "" or "latest") and
// decrypts it to dest, a .tar.gz. It returns the backup it fetched.
func (c *Config) Fetch(ctx context.Context, key []byte, obj, dest string) (*Object, error) {
	objs, err := c.Backups(ctx)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("there are no backups in %s/%s", c.Bucket, c.Prefix)
	}
	pick := &objs[len(objs)-1]
	if obj != "" && obj != "latest" {
		pick = nil
		for i, o := range objs {
			if o.Key == obj || strings.TrimPrefix(o.Key, c.Prefix) == obj {
				pick = &objs[i]
			}
		}
		if pick == nil {
			return nil, fmt.Errorf("no backup %q (berth offsite:status lists them)", obj)
		}
	}
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("%s already exists", dest)
	}
	rc, err := c.Client().Get(ctx, pick.Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	err = Decrypt(f, rc, key)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return pick, os.Rename(tmp, dest)
}
