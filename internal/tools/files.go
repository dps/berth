package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	fileLife  = 24 * time.Hour
	maxFile   = 256 << 20 // the most any one file or provider answer can be
	maxUpload = 64 << 20
	maxStore  = 4 << 30 // the oldest files go first past this
)

// FileRef describes a stored file. Tools return it in place of bytes.
type FileRef struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	Expires     time.Time `json:"expires"`
}

type fileMeta struct {
	App         string    `json:"app"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	Expires     time.Time `json:"expires"`
}

// Files keeps files made by tools or uploaded by apps, for a day. They
// belong to the app that made them.
type Files struct {
	dir  string
	base func() string
	mu   sync.Mutex
}

func newFiles(dir string) *Files { return &Files{dir: dir, base: func() string { return "" }} }

var fileID = regexp.MustCompile(`^file_[0-9a-f]{32}$`)

func (f *Files) paths(id string) (data, meta string) {
	return filepath.Join(f.dir, id+".bin"), filepath.Join(f.dir, id+".json")
}

// Put stores a file for app.
func (f *Files) Put(app, contentType string, data []byte) (*FileRef, error) {
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := "file_" + hex.EncodeToString(b)
	m := fileMeta{app, contentType, int64(len(data)), time.Now().Add(fileLife)}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return nil, err
	}
	dp, mp := f.paths(id)
	if err := os.WriteFile(dp, data, 0o600); err != nil {
		return nil, err
	}
	js, _ := json.Marshal(m)
	if err := os.WriteFile(mp, js, 0o600); err != nil {
		os.Remove(dp)
		return nil, err
	}
	return f.ref(id, m), nil
}

func (f *Files) ref(id string, m fileMeta) *FileRef {
	return &FileRef{ID: id, URL: f.base() + "/v1/files/" + id, ContentType: m.ContentType, Bytes: m.Bytes, Expires: m.Expires}
}

// open finds a file app may read. The owner may read any.
func (f *Files) open(app, id string) (string, fileMeta, bool) {
	var m fileMeta
	if !fileID.MatchString(id) {
		return "", m, false
	}
	dp, mp := f.paths(id)
	b, err := os.ReadFile(mp)
	if err != nil || json.Unmarshal(b, &m) != nil || time.Now().After(m.Expires) || (m.App != app && app != Owner) {
		return "", m, false
	}
	return dp, m, true
}

// sweep removes expired files, then the oldest past the size limit.
func (f *Files) sweep() {
	f.mu.Lock()
	defer f.mu.Unlock()
	metas, _ := filepath.Glob(filepath.Join(f.dir, "file_*.json"))
	type entry struct {
		id string
		m  fileMeta
	}
	var live []entry
	var total int64
	now := time.Now()
	for _, mp := range metas {
		id := strings.TrimSuffix(filepath.Base(mp), ".json")
		var m fileMeta
		b, err := os.ReadFile(mp)
		if err != nil || json.Unmarshal(b, &m) != nil || now.After(m.Expires) {
			f.remove(id)
			continue
		}
		live = append(live, entry{id, m})
		total += m.Bytes
	}
	slices.SortFunc(live, func(a, b entry) int { return a.m.Expires.Compare(b.m.Expires) })
	for _, e := range live {
		if total <= maxStore {
			break
		}
		f.remove(e.id)
		total -= e.m.Bytes
	}
}

func (f *Files) remove(id string) {
	dp, mp := f.paths(id)
	os.Remove(dp)
	os.Remove(mp)
}

func (g *Gateway) serveUpload(w http.ResponseWriter, r *http.Request, app App) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		writeError(w, errorf(http.StatusRequestEntityTooLarge, "too_large", "an upload can be at most 64 MB"))
		return
	}
	if len(body) == 0 {
		writeError(w, badInput("send the file as the request body"))
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" {
		ct = "" // curl's default, not a claim about the file
	}
	ref, err := g.Files.Put(app.Name, ct, body)
	if err != nil {
		writeError(w, errorf(http.StatusInternalServerError, "storage", "couldn't store the file"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": ref})
}

func (g *Gateway) serveFile(w http.ResponseWriter, r *http.Request, app App, id string) {
	path, m, ok := g.Files.open(app.Name, id)
	if !ok {
		writeError(w, notFound("no file %s; files last 24 hours", id))
		return
	}
	w.Header().Set("Content-Type", m.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}

// media reads a file argument: an http(s) URL, a data: URI or a stored
// file's id. A URL goes through the public client, so an app can't use a
// tool to read from the Mac's own network.
func (c *Call) media(ctx context.Context, args map[string]any, name string, limit int64) ([]byte, string, error) {
	v := str(args, name)
	tooBig := errorf(http.StatusRequestEntityTooLarge, "too_large", "%s can be at most %d MB", name, limit>>20)
	switch {
	case strings.HasPrefix(v, "file_"):
		path, m, ok := c.Files.open(c.App, v)
		if !ok {
			return nil, "", notFound("no file %s; files last 24 hours", v)
		}
		if m.Bytes > limit {
			return nil, "", tooBig
		}
		b, err := os.ReadFile(path)
		return b, m.ContentType, err
	case strings.HasPrefix(v, "data:"):
		head, payload, ok := strings.Cut(strings.TrimPrefix(v, "data:"), ",")
		ct, isB64 := strings.CutSuffix(head, ";base64")
		if !ok || !isB64 {
			return nil, "", badInput("%s: a data: URI must be base64", name)
		}
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", badInput("%s: bad base64", name)
		}
		if int64(len(b)) > limit {
			return nil, "", tooBig
		}
		return b, ct, nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", badInput("%s must be an http(s) URL, a data: URI or a file id", name)
	}
	u.User = nil
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", badInput("%s must be a URL", name)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Public.Do(req)
	if err != nil {
		if errors.Is(err, errPrivate) {
			return nil, "", errorf(http.StatusForbidden, "private_address", "%s must be on the public internet", name)
		}
		return nil, "", errorf(http.StatusBadGateway, "fetch_failed", "couldn't fetch %s from %s", name, u.Host)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", errorf(http.StatusBadGateway, "fetch_failed", "%s answered %d for %s", u.Host, resp.StatusCode, name)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", errorf(http.StatusBadGateway, "fetch_failed", "couldn't read %s from %s", name, u.Host)
	}
	if int64(len(b)) > limit {
		return nil, "", tooBig
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct == "" || ct == "application/octet-stream" {
		ct, _, _ = mime.ParseMediaType(http.DetectContentType(b))
	}
	return b, ct, nil
}
