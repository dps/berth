// Package gitx is the small slice of git Berth needs: a bare mirror per app,
// authenticated fetches that never put tokens in argv, and exporting one
// commit's tree into a build directory.
package gitx

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Auth carries credentials for fetches. Token is a GitHub token sent as an
// HTTP extra header via GIT_CONFIG_* env vars (not argv, so it never shows
// in ps); SSHKey is a deploy key path.
type Auth struct {
	Token      string
	SSHKey     string
	KnownHosts string
}

func (a Auth) env() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	if a.Token != "" {
		hdr := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+a.Token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0="+hdr,
		)
	}
	if a.SSHKey != "" {
		kh := a.KnownHosts
		if kh == "" {
			kh = "/dev/null"
		}
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %q -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%q", a.SSHKey, kh))
	}
	return env
}

func git(ctx context.Context, auth Auth, args ...string) (string, error) {
	start := time.Now()
	c := exec.CommandContext(ctx, "git", args...)
	c.Env = auth.env()
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	slog.Debug("exec", "cmd", "git "+strings.Join(args, " "), "dur", time.Since(start).Round(time.Millisecond), "err", err)
	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[len(args)-1], strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// EnsureMirror creates the bare repo if needed and points origin at url.
func EnsureMirror(ctx context.Context, dir, url string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if _, err := git(ctx, Auth{}, "init", "--bare", "--quiet", dir); err != nil {
			return err
		}
		// Pushes from `git push berth` must be accepted into refs/heads.
		git(ctx, Auth{}, "--git-dir", dir, "config", "receive.denyCurrentBranch", "ignore")
	}
	if url == "" {
		return nil
	}
	if cur, err := git(ctx, Auth{}, "--git-dir", dir, "remote", "get-url", "origin"); err == nil {
		if cur == url {
			return nil
		}
		_, err := git(ctx, Auth{}, "--git-dir", dir, "remote", "set-url", "origin", url)
		return err
	}
	_, err := git(ctx, Auth{}, "--git-dir", dir, "remote", "add", "origin", url)
	return err
}

// Fetch updates refs/remotes/origin/<branch> and returns its sha. Remote
// refs never overwrite refs/heads, so branches pushed straight to the Mac
// (`git push berth`) are left alone.
func Fetch(ctx context.Context, dir, branch string, auth Auth) (string, error) {
	ref := "refs/remotes/origin/" + branch
	if _, err := git(ctx, auth, "--git-dir", dir, "fetch", "--quiet", "--prune", "--no-tags", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return "", err
	}
	return git(ctx, Auth{}, "--git-dir", dir, "rev-parse", ref)
}

// FetchSHA fetches a specific commit (for `berth deploy --ref <sha>`).
func FetchSHA(ctx context.Context, dir, sha string, auth Auth) error {
	_, err := git(ctx, auth, "--git-dir", dir, "fetch", "--quiet", "--no-tags", "origin", sha)
	return err
}

// Resolve turns a ref or (abbreviated) sha into a full commit sha. It tries
// remote branches first, then local ones (pushed via `git push berth`).
func Resolve(ctx context.Context, dir, ref string) (string, error) {
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("unknown ref %q", ref)
	}
	for _, cand := range []string{"refs/remotes/origin/" + ref, "refs/heads/" + ref, ref} {
		if sha, err := git(ctx, Auth{}, "--git-dir", dir, "rev-parse", "--verify", "--quiet", cand+"^{commit}"); err == nil && sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("unknown ref %q", ref)
}

// Export writes the tree of sha (optionally a subdirectory) into dest.
func Export(ctx context.Context, dir, sha, subpath, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	treeish := sha
	if subpath != "" {
		treeish = sha + ":" + strings.Trim(subpath, "/")
	}
	archive := exec.CommandContext(ctx, "git", "--git-dir", dir, "archive", "--format=tar", treeish)
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", dest)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	var aerr, terr bytes.Buffer
	archive.Stderr, untar.Stderr = &aerr, &terr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := archive.Run(); err != nil {
		untar.Wait()
		return fmt.Errorf("git archive %s: %s", treeish, strings.TrimSpace(aerr.String()))
	}
	if err := untar.Wait(); err != nil {
		return fmt.Errorf("tar: %s", strings.TrimSpace(terr.String()))
	}
	return nil
}

func Subject(ctx context.Context, dir, sha string) string {
	s, _ := git(ctx, Auth{}, "--git-dir", dir, "log", "-1", "--format=%s", sha)
	return s
}

// Touches reports whether any commit in old..new changed files under path.
// With no old sha (first deploy) or no path it is always true.
func Touches(ctx context.Context, dir, old, new, path string) bool {
	if path == "" || old == "" {
		return true
	}
	out, err := git(ctx, Auth{}, "--git-dir", dir, "diff", "--name-only", old, new, "--", strings.Trim(path, "/"))
	if err != nil {
		return true
	}
	return out != ""
}

// LsRemote returns the sha of branch on url (polling for non-GitHub repos).
func LsRemote(ctx context.Context, url, branch string, auth Auth) (string, error) {
	if strings.HasPrefix(url, "-") {
		return "", fmt.Errorf("invalid repository %q", url)
	}
	out, err := git(ctx, auth, "ls-remote", url, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return "", fmt.Errorf("branch %s not found on %s", branch, url)
	}
	return f[0], nil
}

// shellQuote wraps s in single quotes for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InstallPostReceive writes a hook that hands pushes of branch to berth.
// Every interpolated value is single-quoted for the shell (branch and app are
// also validated at creation, but the hook must be safe on its own).
func InstallPostReceive(dir, berthBin, berthHome, app, branch string) error {
	hook := fmt.Sprintf(`#!/bin/sh
# Installed by berth: deploy pushes to the app's branch.
export BERTH_HOME=%[1]s
want=%[3]s
while read old new ref; do
  if [ "$ref" = "$want" ] && [ "$new" != "0000000000000000000000000000000000000000" ]; then
    %[2]s deploy %[4]s --ref "$new"
  fi
done
`, shellQuote(berthHome), shellQuote(berthBin), shellQuote("refs/heads/"+branch), shellQuote(app))
	return os.WriteFile(filepath.Join(dir, "hooks", "post-receive"), []byte(hook), 0o755)
}

// FetchRef fetches the remote ref src (such as refs/pull/12/head) into the
// local ref dst and returns its sha.
func FetchRef(ctx context.Context, dir, src, dst string, auth Auth) (string, error) {
	if _, err := git(ctx, auth, "--git-dir", dir, "fetch", "--quiet", "--no-tags", "origin", "+"+src+":"+dst); err != nil {
		return "", err
	}
	return git(ctx, Auth{}, "--git-dir", dir, "rev-parse", dst)
}
