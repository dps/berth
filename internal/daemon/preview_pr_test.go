package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/state"
)

// fakeGitHub serves the pull request list it's given and records statuses
// and comments.
type fakeGitHub struct {
	mu       sync.Mutex
	pulls    []map[string]any
	statuses []string // "<sha7> <state>"
	comments []string
}

func (f *fakeGitHub) pr(n int, sha, headRepo, base string) map[string]any {
	return map[string]any{"number": n, "title": "change", "head": map[string]any{"sha": sha, "ref": "feature", "repo": map[string]any{"full_name": headRepo}}, "base": map[string]any{"ref": base}}
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/repos/o/web/pulls":
		json.NewEncoder(w).Encode(f.pulls)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/o/web/statuses/"):
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.statuses = append(f.statuses, strings.TrimPrefix(r.URL.Path, "/repos/o/web/statuses/")[:7]+" "+b["state"])
		w.WriteHeader(201)
		io.WriteString(w, "{}")
	case r.Method == "POST" && r.URL.Path == "/repos/o/web/issues/7/comments":
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.comments = append(f.comments, b["body"])
		w.WriteHeader(201)
		io.WriteString(w, "{}")
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 404)
	}
}

func TestPullRequestPreviews(t *testing.T) {
	d, rt, repo := newTestDaemon(t)
	gitIn := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	// A pull request branch, published the way GitHub does: refs/pull/7/head.
	gitIn("checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("1"), 0o644)
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "feature")
	sha1 := gitIn("rev-parse", "HEAD")
	gitIn("update-ref", "refs/pull/7/head", sha1)
	gitIn("update-ref", "refs/pull/8/head", sha1)
	gitIn("checkout", "-q", "main")
	// github.com/o/web is this local repo.
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gitconfig, []byte("[url \""+repo+"\"]\n\tinsteadOf = https://github.com/o/web.git\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)

	gh := &fakeGitHub{}
	srv := httptest.NewServer(http.HandlerFunc(gh.serve))
	defer srv.Close()
	d.GitHubAPI = func(tok string) *github.Client { c := github.New(tok); c.BaseURL = srv.URL; return c }
	os.MkdirAll(d.Store.SecretsDir(), 0o700)
	os.WriteFile(filepath.Join(d.Store.SecretsDir(), "github.token"), []byte("tok"), 0o600)
	d.Store.SaveApp(&state.App{Name: "web", Repo: "o/web", Branch: "main", PreviewsOn: true})
	d.Store.SaveEnv("web", state.Env{"GREETING": "hi"})

	gh.pulls = []map[string]any{
		gh.pr(7, sha1, "o/web", "main"),
		gh.pr(8, sha1, "stranger/web", "main"), // a fork: never previewed
		gh.pr(9, sha1, "o/web", "release"),     // into another branch
	}
	if err := d.syncPreviews(d.ctx, "web"); err != nil {
		t.Fatal(err)
	}
	pv, err := d.Store.App("web-pr-7")
	if err != nil || pv.DevOf != "web" || pv.Preview != 7 {
		t.Fatalf("preview %+v %v", pv, err)
	}
	if d.Store.AppExists("web-pr-8") || d.Store.AppExists("web-pr-9") {
		t.Fatal("previewed a fork or another branch's PR")
	}
	cur := d.Store.CurrentRelease("web-pr-7")
	if cur == nil || cur.SHA != sha1 || cur.Status != state.StatusRunning {
		t.Fatalf("preview release %+v", cur)
	}
	if env, _ := d.Store.Env("web-pr-7"); env["GREETING"] != "hi" {
		t.Fatalf("config not copied: %v", env)
	}
	if strings.Join(gh.statuses, ",") != sha1[:7]+" pending,"+sha1[:7]+" success" || len(gh.comments) != 1 || !strings.Contains(gh.comments[0], "https://web-pr-7.") || !strings.Contains(gh.comments[0], "LAN only") {
		t.Fatalf("statuses %v comments %v", gh.statuses, gh.comments)
	}

	// Nothing new: nothing happens.
	builds := len(rt.CallLog("build"))
	d.syncPreviews(d.ctx, "web")
	if len(rt.CallLog("build")) != builds || len(gh.comments) != 1 {
		t.Fatal("rebuilt an unchanged pull request")
	}

	// A push to the PR redeploys it, without another comment.
	gitIn("checkout", "-q", "feature")
	os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("2"), 0o644)
	gitIn("commit", "-q", "-am", "more")
	sha2 := gitIn("rev-parse", "HEAD")
	gitIn("update-ref", "refs/pull/7/head", sha2)
	gitIn("checkout", "-q", "main")
	gh.pulls[0] = gh.pr(7, sha2, "o/web", "main")
	d.syncPreviews(d.ctx, "web")
	if cur := d.Store.CurrentRelease("web-pr-7"); cur.SHA != sha2 || len(gh.comments) != 1 {
		t.Fatalf("after a push: %s, %d comments", cur.SHA, len(gh.comments))
	}

	// Over the limit: a status says why, once.
	d.updateApp("web", func(a *state.App) { a.PreviewMax = 1 })
	gitIn("update-ref", "refs/pull/10/head", sha1)
	gh.pulls = append(gh.pulls, gh.pr(10, sha1, "o/web", "main"))
	d.syncPreviews(d.ctx, "web")
	d.syncPreviews(d.ctx, "web")
	if d.Store.AppExists("web-pr-10") || strings.Count(strings.Join(gh.statuses, ","), sha1[:7]+" error") != 1 {
		t.Fatalf("limit: statuses %v", gh.statuses)
	}

	// Listing.
	b, _ := json.Marshal(PreviewParams{App: "web"})
	res, err := d.hPreviews(d.ctx, b, io.Discard)
	if l := res.(PreviewList); err != nil || len(l.Previews) != 1 || l.Previews[0].PR != 7 || l.Previews[0].Status != state.StatusRunning {
		t.Fatalf("list %+v %v", res, err)
	}

	// Closing the PR removes the preview.
	gh.pulls = nil
	d.syncPreviews(d.ctx, "web")
	if d.Store.AppExists("web-pr-7") {
		t.Fatal("preview left after its PR closed")
	}

	// Preview-shaped names are kept for previews.
	cb, _ := json.Marshal(CreateParams{Name: "shop-pr-3", Repo: repo, NoDeploy: true})
	if _, err := d.hCreate(d.ctx, cb, io.Discard); err == nil {
		t.Fatal("created an app with a preview's name")
	}
	// Previews need a GitHub repo.
	d.Store.SaveApp(&state.App{Name: "local", Repo: repo, Branch: "main"})
	b, _ = json.Marshal(PreviewParams{App: "local", On: true})
	if _, err := d.hPreviews(d.ctx, b, io.Discard); err == nil {
		t.Fatal("previews on for a non-GitHub app")
	}
	d.stopTails()
}
