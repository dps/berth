package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotTakesTheWorkingTreeWithoutTouchingIt(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) { os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644) }
	git("init", "-q", "-b", "main")
	write("app.js", "v1\n")
	write(".gitignore", ".env\nnode_modules/\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	head := git("rev-parse", "HEAD")

	clean, err := takeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if clean.SHA != head || clean.Changes != "" {
		t.Fatalf("clean tree: %+v", clean)
	}

	write("app.js", "v2\n")    // modified, unstaged
	write("new.js", "hello\n") // untracked
	write("staged.js", "s\n")
	git("add", "staged.js") // staged
	write(".env", "SECRET=1\n")
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	write("node_modules/x.js", "x\n")
	statusBefore := git("status", "--porcelain")

	snap, err := takeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SHA == head || snap.Base != head || !strings.Contains(snap.Changes, "3 files changed") {
		t.Fatalf("snapshot %+v", snap)
	}
	files := git("ls-tree", "-r", "--name-only", snap.SHA)
	for _, want := range []string{"app.js", "new.js", "staged.js", ".gitignore"} {
		if !strings.Contains(files, want) {
			t.Errorf("snapshot is missing %s", want)
		}
	}
	for _, f := range strings.Split(files, "\n") {
		if f == ".env" || strings.HasPrefix(f, "node_modules/") {
			t.Errorf("snapshot has ignored file %s", f)
		}
	}
	if got := git("show", snap.SHA+":app.js"); got != "v2" {
		t.Errorf("app.js in snapshot = %q", got)
	}
	if parent := git("rev-parse", snap.SHA+"^"); parent != head {
		t.Errorf("snapshot parent %s", parent)
	}
	// The developer's branch, index and working tree are as they were.
	if git("rev-parse", "HEAD") != head || git("status", "--porcelain") != statusBefore {
		t.Fatal("snapshot changed the repo")
	}
}
