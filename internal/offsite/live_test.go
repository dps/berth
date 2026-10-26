package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveS3 runs against a real S3-compatible service when
// BERTH_S3_TEST=endpoint,bucket,access-key,secret is set, e.g. rclone's:
//
//	container run -d -p 127.0.0.1:19000:9000 --entrypoint sh rclone/rclone:1.71 -c \
//	  "mkdir -p /data/b && exec rclone serve s3 --auth-key k,s --addr :9000 /data"
//	BERTH_S3_TEST=http://127.0.0.1:19000,b,k,s go test ./internal/offsite -run Live
func TestLiveS3(t *testing.T) {
	spec := os.Getenv("BERTH_S3_TEST")
	if spec == "" {
		t.Skip("BERTH_S3_TEST not set")
	}
	f := strings.Split(spec, ",")
	cfg := &Config{Endpoint: f[0], Bucket: f[1], AccessKey: f[2], SecretKey: f[3], Prefix: "berth/live-test/", Keep: 2}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := cfg.Client()
	old := partSize
	partSize = 5 << 20 // S3's smallest part
	defer func() { partSize = old }()
	key := NewKey()
	dir := t.TempDir()
	for i, size := range []int{1000, 12<<20 + 3} { // a single PUT, then three parts
		plain := make([]byte, size)
		rand.Read(plain)
		enc := filepath.Join(dir, "enc")
		f, _ := os.Create(enc)
		if err := Encrypt(f, bytes.NewReader(plain), key); err != nil {
			t.Fatal(err)
		}
		f.Close()
		name := cfg.ObjectName(time.Date(2026, 10, 25, 3, 0, i, 0, time.UTC))
		if err := c.PutFile(ctx, name, enc); err != nil {
			t.Fatalf("put %d bytes: %v", size, err)
		}
		rc, err := c.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		err = Decrypt(&got, rc, key)
		rc.Close()
		if err != nil || !bytes.Equal(got.Bytes(), plain) {
			t.Fatalf("size %d: round trip: %v", size, err)
		}
	}
	// A third makes Prune drop the oldest.
	os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600)
	c.PutFile(ctx, cfg.ObjectName(time.Date(2026, 10, 25, 3, 0, 9, 0, time.UTC)), filepath.Join(dir, "x"))
	gone, err := cfg.Prune(ctx)
	if err != nil || len(gone) != 1 || !strings.HasSuffix(gone[0], "030000.tar.gz.enc") {
		t.Fatalf("prune %v %v", gone, err)
	}
	objs, err := cfg.Backups(ctx)
	if err != nil || len(objs) != 2 {
		t.Fatalf("backups %+v %v", objs, err)
	}
	for _, o := range objs {
		if err := c.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
	// Wrong secret: refused.
	bad := *c
	bad.SecretKey = "wrong"
	if _, err := bad.List(ctx, ""); err == nil {
		t.Fatal("wrong secret accepted")
	}
}
