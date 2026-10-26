package daemon

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dps/berth/internal/offsite"
)

// memS3 is just enough S3 for single-part uploads.
func memS3(t *testing.T) (*httptest.Server, map[string][]byte) {
	var mu sync.Mutex
	objs := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/") {
			http.Error(w, "<Error><Code>InvalidAccessKeyId</Code><Message>no</Message></Error>", 403)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bkt/")
		switch {
		case r.Method == "GET" && r.URL.Path == "/bkt":
			var keys []string
			for k := range objs {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			fmt.Fprint(w, "<ListBucketResult>")
			for _, k := range keys {
				fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>", k, len(objs[k]), time.Now().UTC().Format(time.RFC3339))
			}
			fmt.Fprint(w, "</ListBucketResult>")
		case r.Method == "PUT":
			objs[key], _ = io.ReadAll(r.Body)
		case r.Method == "GET":
			b, ok := objs[key]
			if !ok {
				http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", 404)
				return
			}
			w.Write(b)
		case r.Method == "DELETE":
			delete(objs, key)
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, objs
}

func TestOffsiteBackups(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	srv, objs := memS3(t)
	call := func(h handler, p any) (any, string, error) {
		b, _ := json.Marshal(p)
		var out strings.Builder
		res, err := h(d.ctx, b, &out)
		return res, out.String(), err
	}
	setup := OffsiteParams{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AKID", SecretKey: "s", Keep: 1}
	if _, _, err := call(d.hOffsiteSetup, OffsiteParams{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "wrong", SecretKey: "s"}); err == nil || !strings.Contains(err.Error(), "InvalidAccessKeyId") {
		t.Fatalf("bad credentials: %v", err)
	}
	if _, _, err := call(d.hOffsiteSetup, OffsiteParams{Endpoint: "ftp://x", Bucket: "bkt", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Fatal("ftp endpoint accepted")
	}
	res, _, err := call(d.hOffsiteSetup, setup)
	if err != nil {
		t.Fatal(err)
	}
	st := res.(OffsiteStatus)
	if !st.NewKey || !strings.HasPrefix(st.Recovery, "berthrestore_") || !strings.HasPrefix(st.Prefix, "berth/") || len(objs) != 0 {
		t.Fatalf("setup %+v, objects %v", st, objs)
	}
	// Setting up again keeps the key; a different key is refused.
	if res, _, _ := call(d.hOffsiteSetup, setup); res.(OffsiteStatus).NewKey {
		t.Fatal("setup made a second key")
	}
	other := setup
	other.Key = offsite.EncodeKey(offsite.NewKey())
	if _, _, err := call(d.hOffsiteSetup, other); err == nil {
		t.Fatal("replaced the backup key")
	}

	os.WriteFile(d.Store.Path("marker.txt"), []byte("hello"), 0o600)
	if _, out, err := call(d.hOffsiteRun, nil); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(objs) != 1 {
		t.Fatalf("objects %v", objs)
	}
	for k, v := range objs {
		if !strings.HasPrefix(string(v), "BERTHENC1") || strings.Contains(string(v), "hello") {
			t.Fatalf("%s isn't encrypted", k)
		}
	}
	time.Sleep(1100 * time.Millisecond) // a new name each second
	if _, out, err := call(d.hOffsiteRun, nil); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(objs) != 1 { // keep 1
		t.Fatalf("not pruned: %v", objs)
	}
	if left, _ := os.ReadDir(d.Store.Path("backups", "offsite")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
	res, _, err = call(d.hOffsiteStatus, nil)
	if s := res.(OffsiteStatus); err != nil || len(s.Backups) != 1 || s.Last.LastOK.IsZero() || s.Last.LastError != "" {
		t.Fatalf("status %+v %v", res, err)
	}

	// Download, decrypt, and find the files inside.
	dest := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, out, err := call(d.hOffsiteGet, OffsiteParams{Dest: dest}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f, _ := os.Open(dest)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		found[h.Name] = true
	}
	if !found["marker.txt"] || !found["config.json"] || found["backups/offsite/"] {
		t.Fatalf("archive has %v", found)
	}
	// A wrong key can't read it.
	if _, _, err := call(d.hOffsiteGet, OffsiteParams{Dest: dest + "2", Key: offsite.EncodeKey(offsite.NewKey())}); err == nil {
		t.Fatal("decrypted with the wrong key")
	}

	// Failures are recorded and warned about once two days pass.
	srv.Close()
	if _, _, err := call(d.hOffsiteRun, nil); err == nil {
		t.Fatal("upload to a dead server worked")
	}
	s := offsite.LoadStatus(d.Store.Root)
	s.LastOK = time.Now().Add(-72 * time.Hour)
	offsite.SaveStatus(d.Store.Root, s)
	if w := d.offsiteWarning(); !strings.Contains(w, "off-Mac backup") {
		t.Fatalf("warning %q", w)
	}
}
