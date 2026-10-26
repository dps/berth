package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
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
)

// AWS's worked example for GET Object (Signature Version 4 docs).
func TestSignMatchesAWSExample(t *testing.T) {
	c := &S3{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	c.sign(req, emptySHA256)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// fakeS3 keeps objects in memory and supports multipart uploads.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	parts    map[string]map[int][]byte
	unsigned int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") {
		f.unsigned++
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", 403)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && r.URL.Path == "/bucket":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, "<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-10-25T03:00:00.000Z</LastModified></Contents>", k, len(f.objects[k]))
		}
		fmt.Fprint(w, "<IsTruncated>false</IsTruncated></ListBucketResult>")
	case r.Method == "POST" && q.Has("uploads"):
		f.parts[key] = map[int][]byte{}
		fmt.Fprint(w, "<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>")
	case r.Method == "PUT" && q.Get("uploadId") != "":
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		b, _ := io.ReadAll(r.Body)
		f.parts[key][n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"etag%d"`, n))
	case r.Method == "POST" && q.Get("uploadId") != "":
		var done struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		xml.NewDecoder(r.Body).Decode(&done)
		var all []byte
		for _, p := range done.Parts {
			all = append(all, f.parts[key][p.Number]...)
		}
		f.objects[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	case r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
	case r.Method == "GET":
		b, ok := f.objects[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>", 404)
			return
		}
		w.Write(b)
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	}
}

func newFakeS3(t *testing.T) (*fakeS3, *S3) {
	f := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &S3{Endpoint: srv.URL, Region: "auto", Bucket: "bucket", AccessKey: "key", SecretKey: "secret"}
}

func TestS3PutListGetDelete(t *testing.T) {
	f, c := newFakeS3(t)
	ctx := context.Background()
	old := partSize
	partSize = 1000
	defer func() { partSize = old }()
	dir := t.TempDir()
	for _, size := range []int{10, 2500} { // one PUT, then three parts
		data := make([]byte, size)
		rand.Read(data)
		p := filepath.Join(dir, "f")
		os.WriteFile(p, data, 0o600)
		key := fmt.Sprintf("berth/mini/b %d.enc", size)
		if err := c.PutFile(ctx, key, p); err != nil {
			t.Fatal(err)
		}
		rc, err := c.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip differs", size)
		}
	}
	objs, err := c.List(ctx, "berth/mini/")
	if err != nil || len(objs) != 2 || objs[1].Size != 2500 {
		t.Fatalf("list %+v %v", objs, err)
	}
	if err := c.Delete(ctx, objs[0].Key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, objs[0].Key); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("deleted object: %v", err)
	}
	if f.unsigned != 0 {
		t.Fatal("unsigned requests")
	}
}
