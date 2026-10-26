package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dps/berth/internal/detect"
)

func TestStartersFollowTheContract(t *testing.T) {
	for name := range stacks {
		dir := t.TempDir()
		d := newAgentsData("demo", &BerthInfo{Domain: "example.com", AI: true})
		d.Stack = stacks[name]
		files, err := writeStarter(dir, name, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range append(files, ".gitignore", "AGENTS.md", "CLAUDE.md", "README.md") {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if s := string(b); strings.Contains(s, "{{") || strings.Contains(s, "<no value>") {
				t.Fatalf("%s/%s isn't fully rendered:\n%s", name, f, s)
			}
		}
		// Berth recognises the starter without a Dockerfile.
		res, err := detect.Detect(dir)
		if err != nil || res.Stack != name {
			t.Fatalf("%s starter detected as %v (%v)", name, res, err)
		}
	}
}

func TestAgentsMD(t *testing.T) {
	d := newAgentsData("demo", &BerthInfo{Domain: "example.com", SignIn: true})
	d.Stack = stacks["node"]
	md := renderAgents(d, "agents")
	for _, want := range []string{"# demo", "https://demo.example.com", "https://demo-dev.example.com", "berth dev:push", "berth logs demo-dev", "### Sign-in", "berth access demo public", "npm run dev"} {
		if !strings.Contains(md, want) {
			t.Errorf("AGENTS.md has no %q", want)
		}
	}
	if strings.Contains(md, "### AI") {
		t.Error("AGENTS.md mentions AI, which isn't set up")
	}
	// Without a Berth to ask, the URLs are placeholders; LAN-only uses .local.
	if md := renderAgents(newAgentsData("demo", nil), "berth"); !strings.Contains(md, "https://demo.<your domain>") {
		t.Errorf("no placeholder URL:\n%s", md)
	}
	if d := newAgentsData("demo", &BerthInfo{Hostname: "mini"}); d.Prod != "https://demo.mini.local" {
		t.Errorf("LAN URL %s", d.Prod)
	}
}

func TestParseSSHTarget(t *testing.T) {
	for in, want := range map[string]sshTarget{
		"mini":                             {dst: "mini"},
		"me@mini.local":                    {dst: "me@mini.local"},
		"ssh://me@mini.local/x/repo.git":   {dst: "me@mini.local"},
		"ssh://me@mini.local:2222/x/r.git": {dst: "me@mini.local", port: "2222"},
	} {
		got, err := parseSSHTarget(in)
		if err != nil || *got != want {
			t.Errorf("parseSSHTarget(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "ssh://-oProxyCommand=x/r.git", "https://github.com/x/y", "a b"} {
		if _, err := parseSSHTarget(bad); err == nil {
			t.Errorf("parseSSHTarget(%q) accepted", bad)
		}
	}
}

// fakeTools puts fake ssh and gh first on PATH. Each records its calls in
// the returned log; ssh answers like a Berth Mac.
func fakeTools(t *testing.T) (logPath, stdinPath string) {
	bin := t.TempDir()
	logPath, stdinPath = filepath.Join(bin, "log"), filepath.Join(bin, "stdin")
	ssh := `#!/bin/sh
for a; do last="$a"; done
printf 'ssh %s\n' "$*" >> "$FAKE_LOG"
case "$last" in
  *"berth 'info'"*) echo '{"version":"t","domain":"example.com","hostname":"mini","signin":true}';;
  *"'git:remote'"*) echo "ssh://me@mini.local/Users/me/.berth/apps/demo/repo.git";;
  *"'config:import'"*) cat > "$FAKE_STDIN";;
esac
`
	gh := `#!/bin/sh
printf 'gh %s\n' "$*" >> "$FAKE_LOG"
[ "$1" = api ] && echo tester
exit 0
`
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755)
	os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", logPath)
	t.Setenv("FAKE_STDIN", stdinPath)
	t.Setenv("BERTH_HOME", t.TempDir()) // this machine isn't a Berth Mac
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	return logPath, stdinPath
}

func readLog(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestCommandsRunOnTheBerthMacOverSSH(t *testing.T) {
	logPath, stdinPath := fakeTools(t)
	t.Chdir(t.TempDir())

	// Nowhere to send it: a clear error, not "run berth init".
	t.Setenv("BERTH_HOST", "")
	if code, ok := forward(NewRoot(), []string{"logs", "demo"}); !ok || code != 1 {
		t.Fatalf("no BERTH_HOST: %d %v", code, ok)
	}

	t.Setenv("BERTH_HOST", "me@mini.local")
	if code, ok := forward(NewRoot(), []string{"logs", "demo", "-n", "5"}); !ok || code != 0 {
		t.Fatalf("forward: %d %v", code, ok)
	}
	if log := readLog(t, logPath); !strings.Contains(log, "-- me@mini.local PATH=/opt/homebrew/bin:/usr/local/bin:$PATH BERTH_CLIENT_VERSION='dev' berth 'logs' 'demo' '-n' '5'") {
		t.Fatalf("ssh got:\n%s", log)
	}
	// Commands that work here, or reach the Mac themselves, aren't forwarded.
	for _, args := range [][]string{{"new", "x"}, {"dev:push"}, {"git:remote", "x"}, {"logs", "--help"}, {}} {
		if _, ok := forward(NewRoot(), args); ok {
			t.Errorf("forwarded %v", args)
		}
	}
	// config:import sends this machine's file.
	os.WriteFile(".env", []byte("A=1\n"), 0o600)
	if code, _ := forward(NewRoot(), []string{"config:import", "demo", ".env"}); code != 0 {
		t.Fatal("config:import")
	}
	if b, _ := os.ReadFile(stdinPath); string(b) != "A=1\n" || !strings.Contains(readLog(t, logPath), "'config:import' 'demo' '-'") {
		t.Fatalf("config:import sent %q:\n%s", b, readLog(t, logPath))
	}
}

func TestNewFromAnotherMachine(t *testing.T) {
	logPath, _ := fakeTools(t)
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("BERTH_HOST", "me@mini.local")

	root := NewRoot()
	root.SetArgs([]string{"new", "demo", "--stack", "go", "-y"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "demo")
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil || !strings.Contains(string(agents), "https://demo.example.com") || !strings.Contains(string(agents), "### Sign-in") {
		t.Fatalf("AGENTS.md: %v\n%s", err, agents)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "@AGENTS.md\n" {
		t.Fatalf("CLAUDE.md %q", b)
	}
	log := readLog(t, logPath)
	for _, want := range []string{
		"gh repo create tester/demo --private --source . --remote origin --push",
		"berth 'create' 'demo' '--repo' 'tester/demo'",
		"berth 'git:remote' 'demo' '--host' 'mini.local' '--print'",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in:\n%s", want, log)
		}
	}
	out, _ := exec.Command("git", "-C", dir, "remote", "get-url", "berth").Output()
	if strings.TrimSpace(string(out)) != "ssh://me@mini.local/Users/me/.berth/apps/demo/repo.git" {
		t.Fatalf("berth remote %q", out)
	}
	if out, _ := exec.Command("git", "-C", dir, "log", "--format=%s").Output(); strings.TrimSpace(string(out)) != "Start demo with berth new" {
		t.Fatalf("commits %q", out)
	}

	// An existing folder is never overwritten.
	t.Chdir(work)
	root = NewRoot()
	root.SetArgs([]string{"new", "demo", "--no-github"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second new: %v", err)
	}
}
