package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	jobPollGap = 5 * time.Second // apps can ask more often; providers aren't
	jobGiveUp  = time.Hour
	jobLife    = 7 * 24 * time.Hour
)

// job is a tool running on a provider for minutes. Nothing here waits for
// it: each check by the app asks the provider, so a job survives berthd
// restarting, and costs nothing while nobody is asking.
type job struct {
	ID       string          `json:"id"`
	App      string          `json:"app"`
	Tool     string          `json:"tool"`
	Provider string          `json:"provider"`
	Op       string          `json:"op"`
	Status   string          `json:"status"` // running, done or failed
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	Created  time.Time       `json:"created"`
	Polled   time.Time       `json:"polled"`
}

// jobView is what an app sees of a job.
type jobView struct {
	ID     string          `json:"id"`
	Tool   string          `json:"tool"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func (j *job) view() jobView { return jobView{j.ID, j.Tool, j.Status, j.Result, j.Error} }

type jobs struct {
	dir string
	mu  sync.Mutex // one check at a time, so a job isn't downloaded twice
}

func newJobs(dir string) *jobs { return &jobs{dir: dir} }

var jobID = regexp.MustCompile(`^job_[0-9a-f]{32}$`)

func (s *jobs) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *jobs) save(j *job) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(j)
	return os.WriteFile(s.path(j.ID), b, 0o600)
}

func (s *jobs) load(id string) (*job, bool) {
	if !jobID.MatchString(id) {
		return nil, false
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, false
	}
	j := &job{}
	return j, json.Unmarshal(b, j) == nil
}

func (s *jobs) create(app, tool, provider, op string) (*job, error) {
	b := make([]byte, 16)
	rand.Read(b)
	j := &job{ID: "job_" + hex.EncodeToString(b), App: app, Tool: tool, Provider: provider, Op: op, Status: "running", Created: time.Now(), Polled: time.Now()}
	return j, s.save(j)
}

func (s *jobs) sweep() {
	paths, _ := filepath.Glob(filepath.Join(s.dir, "job_*.json"))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > jobLife {
			os.Remove(p)
		}
	}
}

func (g *Gateway) serveJob(w http.ResponseWriter, r *http.Request, app App, id string) {
	j, ok := g.jobs.load(id)
	if !ok || (j.App != app.Name && app.Name != Owner) {
		writeError(w, notFound("no job %s", id))
		return
	}
	if j.Status == "running" {
		g.check(r.Context(), id)
		j, _ = g.jobs.load(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j.view()})
}

// check asks the provider how a running job is doing.
func (g *Gateway) check(ctx context.Context, id string) {
	g.jobs.mu.Lock()
	defer g.jobs.mu.Unlock()
	j, ok := g.jobs.load(id)
	if !ok || j.Status != "running" || time.Since(j.Polled) < jobPollGap {
		return
	}
	j.Polled = time.Now()
	fail := func(msg string) {
		j.Status, j.Error = "failed", msg
	}
	t := Find(j.Tool)
	switch {
	case t == nil || t.Poll[j.Provider] == nil:
		fail("this job's tool is gone")
	case time.Since(j.Created) > jobGiveUp:
		fail("gave up after an hour")
	default:
		g.mu.RLock()
		key := g.keys[j.Provider]
		g.mu.RUnlock()
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		out, err := t.Poll[j.Provider](cctx, g.newCall(j.App, key), j.Op)
		var final *Error
		switch {
		case errors.As(err, &final):
			fail(final.Message)
		case err != nil:
			slog.Warn("tool job check failed", "tool", j.Tool, "provider", j.Provider, "err", err) // try again next time
		case out != nil:
			b, err := json.Marshal(out)
			if err != nil {
				fail("couldn't read the result")
			} else {
				j.Status, j.Result = "done", b
			}
		}
	}
	if err := g.jobs.save(j); err != nil {
		slog.Warn("saving a tool job", "err", err)
	}
}
