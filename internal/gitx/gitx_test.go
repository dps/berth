package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The hook must treat every field as data, even hostile ones.
func TestPostReceiveHookIsShellSafe(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "hooks"), 0o755)
	marker := filepath.Join(dir, "pwned") // what an injection would create (hooks run in dir)
	log := filepath.Join(dir, "calls")
	// A fake "berth", with a hostile name, that records its arguments.
	bin := filepath.Join(dir, "fake berth $(touch pwned)")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+log+"\n"), 0o755)
	branch := `x" ]; touch pwned; #`
	if err := InstallPostReceive(dir, bin, "/tmp/h`touch pwned`", "app", branch); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string) {
		c := exec.Command("sh", filepath.Join(dir, "hooks", "post-receive"))
		c.Dir = dir
		c.Stdin = strings.NewReader(stdin)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("hook: %v %s", err, out)
		}
	}
	run("0000 abc refs/heads/main\n")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook executed injected shell")
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("deployed a push to another branch")
	}
	run("0000 abc refs/heads/" + branch + "\n")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook executed injected shell")
	}
	b, _ := os.ReadFile(log)
	if string(b) != "deploy\napp\n--ref\nabc\n" {
		t.Fatalf("berth called with %q", b)
	}
}
