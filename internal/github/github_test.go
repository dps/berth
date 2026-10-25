package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSignatureVerify(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	sig := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, sig) {
		t.Fatal("valid signature rejected")
	}
	for _, bad := range []string{"", "sha256=", "sha1=abc", Sign("other", body), sig[:len(sig)-1] + "0"} {
		if VerifySignature("s3cret", body, bad) {
			t.Errorf("accepted bad signature %q", bad)
		}
	}
	if VerifySignature("s3cret", append(body, ' '), sig) {
		t.Error("accepted modified body")
	}
}

func TestPushTouches(t *testing.T) {
	p := &PushEvent{}
	p.Commits = append(p.Commits, struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	}{Modified: []string{"services/api/main.go"}})
	if !p.Touches("services/api") || !p.Touches("/services/api/") || !p.Touches("") {
		t.Error("should touch")
	}
	if p.Touches("services/web") || p.Touches("services/ap") {
		t.Error("should not touch")
	}
}

func TestBranchHeadETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"e1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		w.Write([]byte("abc123\n"))
	}))
	defer srv.Close()
	c := New("t")
	c.BaseURL = srv.URL
	sha, etag, nm, err := c.BranchHead(context.Background(), "me/app", "main", "")
	if err != nil || sha != "abc123" || etag != `"e1"` || nm {
		t.Fatalf("%q %q %v %v", sha, etag, nm, err)
	}
	_, _, nm, err = c.BranchHead(context.Background(), "me/app", "main", etag)
	if err != nil || !nm {
		t.Fatalf("expected 304: %v %v", nm, err)
	}
}

func TestCreateHookErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	c := New("t")
	c.BaseURL = srv.URL
	_, err := c.CreateHook(context.Background(), "me/app", "https://hooks.x/github/app", "s")
	if !IsStatus(err, 404) {
		t.Fatalf("want 404 APIError, got %v", err)
	}
}
