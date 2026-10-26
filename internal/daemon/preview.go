package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/gitx"
	"github.com/dps/berth/internal/state"
)

// ---- pull request previews (design.md, Pull request previews) ----
//
// With previews on, each open pull request into the app's branch from a
// branch of the same repo gets <app>-pr-<n>: a dev version built from the
// PR's head, with its own data, behind the same access rule as dev
// versions. A commit status links to it from the PR; closing the PR
// destroys it. Pull requests from forks never get one: that would run a
// stranger's code on the Mac.

func previewRef(pr int) string { return fmt.Sprintf("refs/berth/pr/%d", pr) }

const (
	previewContext    = "berth/preview"
	defaultPreviewMax = 3
	maxPreviewMax     = 20
)

// previewsOf lists parent's previews, by pull request number.
func (d *Daemon) previewsOf(parent string) []*state.App {
	apps, _ := d.Store.Apps()
	var out []*state.App
	for _, a := range apps {
		if a.DevOf == parent && a.Preview > 0 {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Preview < out[j].Preview })
	return out
}

func (d *Daemon) previewLock(app string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.previewMu[app]
	if !ok {
		l = &sync.Mutex{}
		d.previewMu[app] = l
	}
	return l
}

// syncPreviews brings name's previews in line with its open pull requests:
// new and updated ones deploy, closed ones are destroyed. Webhooks call it
// as pull requests change, and polling every few minutes catches anything
// they missed.
func (d *Daemon) syncPreviews(ctx context.Context, name string) error {
	l := d.previewLock(name)
	l.Lock()
	defer l.Unlock()
	parent, err := d.Store.App(name)
	if err != nil || !parent.PreviewsOn {
		return err
	}
	tok := d.githubToken()
	if tok == "" || !parent.IsGitHub() {
		return fmt.Errorf("previews need a GitHub repo and Berth's GitHub login")
	}
	gh := d.GitHubAPI(tok)
	prs, err := gh.OpenPulls(ctx, parent.Repo)
	if err != nil {
		return err
	}
	want := map[int]github.PullRequest{}
	for _, pr := range prs {
		if pr.Base.Ref == parent.Branch && pr.FromRepo(parent.Repo) && pr.Head.SHA != "" {
			want[pr.Number] = pr
		}
	}
	running := 0
	for _, a := range d.previewsOf(parent.Name) {
		if _, open := want[a.Preview]; open {
			running++
			continue
		}
		var buf bytes.Buffer
		d.destroyApp(ctx, a, false, &buf)
		d.event(state.Event{Kind: "preview.closed", App: a.Name, Message: fmt.Sprintf("pull request #%d closed; preview removed", a.Preview)})
	}
	nums := make([]int, 0, len(want))
	for n := range want {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	max := parent.PreviewMax
	if max <= 0 {
		max = defaultPreviewMax
	}
	for _, n := range nums {
		pr := want[n]
		pname := state.PreviewName(parent.Name, n)
		exists := d.Store.AppExists(pname)
		if exists {
			// Already built this commit, whether it worked or not.
			if rs, _ := d.Store.Releases(pname); len(rs) > 0 && rs[0].SHA == pr.Head.SHA {
				continue
			}
		} else if running >= max {
			key := pname + "@" + pr.Head.SHA
			d.mu.Lock()
			noted := d.previewNoted[key]
			d.previewNoted[key] = true
			d.mu.Unlock()
			if !noted {
				msg := fmt.Sprintf("%d previews are running, the most for %s (berth previews:on %s --max %d raises it)", running, parent.Name, parent.Name, max+1)
				gh.SetStatus(ctx, parent.Repo, pr.Head.SHA, "error", "", msg, previewContext)
				d.event(state.Event{Kind: "preview.skipped", App: parent.Name, Message: fmt.Sprintf("#%d: %s", n, msg)})
			}
			continue
		} else {
			running++
		}
		if err := d.deployPreview(ctx, parent, pr, gh, !exists); err != nil {
			slog.Warn("preview", "app", pname, "err", err)
		}
	}
	return nil
}

// deployPreview fetches the pull request's head into the parent's mirror
// and deploys it to the preview, reporting to the PR as a commit status.
func (d *Daemon) deployPreview(ctx context.Context, parent *state.App, pr github.PullRequest, gh *github.Client, created bool) error {
	n := pr.Number
	mirror := d.Store.Path("apps", parent.Name, "repo.git")
	if err := gitx.EnsureMirror(ctx, mirror, parent.CloneURL()); err != nil {
		return err
	}
	sha, err := gitx.FetchRef(ctx, mirror, fmt.Sprintf("refs/pull/%d/head", n), previewRef(n), d.gitAuth(parent))
	if err != nil {
		return fmt.Errorf("fetching pull request #%d: %w", n, err)
	}
	var out bytes.Buffer
	app, err := d.ensureChild(ctx, parent, n, &out)
	if err != nil {
		gh.SetStatus(ctx, parent.Repo, sha, "error", "", "Berth couldn't create the preview: "+err.Error(), previewContext)
		return err
	}
	url := ""
	if urls := AppURLs(d.cfg(), app); len(urls) > 0 {
		url = urls[0]
	}
	gh.SetStatus(ctx, parent.Repo, sha, "pending", url, "Building the preview", previewContext)
	d.event(state.Event{Kind: "preview", App: app.Name, Message: fmt.Sprintf("deploying #%d (%s) at %s", n, pr.Title, sha[:min(7, len(sha))])})
	rel, err := d.Deployer.Deploy(ctx, deploy.Request{App: app.Name, Ref: sha, Trigger: "pr", Out: &out})
	if err != nil {
		stage := "build"
		if rel != nil && rel.FailedStage != "" {
			stage = rel.FailedStage
		}
		gh.SetStatus(ctx, parent.Repo, sha, "failure", "", fmt.Sprintf("Preview failed at %s: berth logs %s --build", stage, app.Name), previewContext)
		d.event(state.Event{Kind: "preview.failed", App: app.Name, Message: fmt.Sprintf("#%d failed at %s: %v", n, stage, err)})
		return err
	}
	gh.SetStatus(ctx, parent.Repo, sha, "success", url, "Preview is live", previewContext)
	if created && url != "" {
		access := "It has its own empty data, and the same access rule as the dev version."
		if a, _ := d.Store.App(app.Name); a != nil && a.Access != nil {
			access = "It has its own empty data, and is open to the @owners group, like the dev version."
		} else if a != nil && a.Private {
			access = "It has its own empty data, and is reachable on the LAN only."
		}
		body := fmt.Sprintf("**Preview:** %s\n\nBerth redeploys it on every push to this pull request and removes it when the pull request closes. %s", url, access)
		if err := gh.Comment(ctx, parent.Repo, n, body); err != nil {
			slog.Warn("preview comment", "app", app.Name, "err", err)
		}
	}
	return nil
}

// ---- RPC ----

type PreviewParams struct {
	App string `json:"app"`
	On  bool   `json:"on,omitempty"`
	Off bool   `json:"off,omitempty"`
	Max int    `json:"max,omitempty"`
}

type PreviewInfo struct {
	PR      int    `json:"pr"`
	App     string `json:"app"`
	URL     string `json:"url,omitempty"`
	Release int    `json:"release,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

type PreviewList struct {
	App      string        `json:"app"`
	On       bool          `json:"on"`
	Max      int           `json:"max"`
	Previews []PreviewInfo `json:"previews"`
}

func (d *Daemon) hPreviews(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p PreviewParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	parent, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	if parent.DevOf != "" {
		return nil, fmt.Errorf("%s is a dev version or preview; previews belong to %s", parent.Name, parent.DevOf)
	}
	switch {
	case p.On:
		if !parent.IsGitHub() {
			return nil, fmt.Errorf("previews follow GitHub pull requests, and %s isn't deployed from a GitHub repo", parent.Name)
		}
		if d.githubToken() == "" {
			return nil, fmt.Errorf("previews need Berth's GitHub login (berth init) to read pull requests and post their status")
		}
		if p.Max < 0 || p.Max > maxPreviewMax {
			return nil, fmt.Errorf("--max is 1 to %d", maxPreviewMax)
		}
		if err := d.updateApp(parent.Name, func(a *state.App) {
			a.PreviewsOn = true
			if p.Max > 0 {
				a.PreviewMax = p.Max
			}
		}); err != nil {
			return nil, err
		}
		if parent.Webhook.ID != 0 {
			if err := d.GitHubAPI(d.githubToken()).SetHookEvents(ctx, parent.Repo, parent.Webhook.ID, github.HookEvents); err != nil {
				fmt.Fprintf(out, "       warning: couldn't add pull requests to the webhook (%v); previews follow by polling every 5 minutes\n", err)
			}
		} else {
			fmt.Fprintf(out, "       %s has no webhook, so previews follow pull requests by polling every 5 minutes\n", parent.Name)
		}
		max := p.Max
		if max == 0 {
			max = parent.PreviewMax
		}
		if max == 0 {
			max = defaultPreviewMax
		}
		fmt.Fprintf(out, "-----> Previews are on for %s: each open pull request from a branch of %s gets its own app, %s-pr-<n> (up to %d at once)\n", parent.Name, parent.Repo, parent.Name, max)
		go func() {
			if err := d.syncPreviews(d.ctx, parent.Name); err != nil {
				slog.Warn("previews", "app", parent.Name, "err", err)
			}
		}()
		d.event(state.Event{Kind: "previews", App: parent.Name, Message: "previews on"})
	case p.Off:
		if err := d.updateApp(parent.Name, func(a *state.App) { a.PreviewsOn = false }); err != nil {
			return nil, err
		}
		for _, a := range d.previewsOf(parent.Name) {
			fmt.Fprintf(out, "-----> Destroying %s\n", a.Name)
			d.destroyApp(ctx, a, false, out)
		}
		fmt.Fprintf(out, "-----> Previews are off for %s\n", parent.Name)
		d.event(state.Event{Kind: "previews", App: parent.Name, Message: "previews off"})
	}
	parent, _ = d.Store.App(p.App)
	res := PreviewList{App: parent.Name, On: parent.PreviewsOn, Max: parent.PreviewMax, Previews: []PreviewInfo{}}
	if res.Max == 0 {
		res.Max = defaultPreviewMax
	}
	cfg := d.cfg()
	for _, a := range d.previewsOf(parent.Name) {
		pi := PreviewInfo{PR: a.Preview, App: a.Name, Status: "new"}
		if urls := AppURLs(cfg, a); len(urls) > 0 {
			pi.URL = urls[0]
		}
		if rs, _ := d.Store.Releases(a.Name); len(rs) > 0 {
			pi.Release, pi.SHA, pi.Status, pi.Error = rs[0].N, rs[0].SHA, rs[0].Status, rs[0].Error
		}
		if a.Stopped {
			pi.Status = "asleep"
		}
		res.Previews = append(res.Previews, pi)
	}
	return res, nil
}
