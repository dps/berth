package state

import (
	"os"
	"strings"
	"testing"
)

func TestValidateAppName(t *testing.T) {
	for _, ok := range []string{"myapp", "a", "my-app-2", strings.Repeat("a", 63)} {
		if err := ValidateAppName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "MyApp", "-x", "x-", "a_b", "hooks", "berth", "www", "pg-db", "redis-x", strings.Repeat("a", 64)} {
		if ValidateAppName(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestEnvRoundTrip(t *testing.T) {
	e := Env{"A": "1", "MULTI": "line1\nline2", "QUOTE": `say "hi"`, "EMPTY": "", "URL": "postgres://u:p@h:1/d?x=y"}
	got, err := ParseEnv(strings.NewReader(e.Format()))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range e {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}

func TestParseDotEnv(t *testing.T) {
	in := "# comment\nexport FOO=bar\nQ=\"quoted value\"\nS='single'\n\nX = spaced \n"
	e, err := ParseEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"FOO": "bar", "Q": "quoted value", "S": "single", "X": "spaced"}
	for k, v := range want {
		if e[k] != v {
			t.Errorf("%s=%q want %q", k, e[k], v)
		}
	}
	if _, err := ParseEnv(strings.NewReader("1BAD=x\n")); err == nil {
		t.Error("expected invalid key error")
	}
}

func TestContainerEnvFileSplitsMultiline(t *testing.T) {
	file, extra := Env{"A": "1", "CERT": "a\nb"}.ContainerEnvFile()
	if file != "A=1\n" || len(extra) != 1 || extra[0] != "CERT=a\nb" {
		t.Fatalf("file=%q extra=%q", file, extra)
	}
}

func TestSecretMasking(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "GH_TOKEN", "SESSION_SECRET", "DB_PASSWORD", "DATABASE_URL"} {
		if !IsSecretKey(k) {
			t.Errorf("%s should be masked", k)
		}
	}
	if IsSecretKey("PORT") || IsSecretKey("NODE_ENV") {
		t.Error("PORT/NODE_ENV should not be masked")
	}
	if m := Mask("sk-abcdefgh"); strings.Contains(m, "abcdef") {
		t.Errorf("mask leaked: %s", m)
	}
}

func TestStoreAtomicAndReleases(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, err := s.Config(); err != ErrNotInitialized {
		t.Fatalf("want ErrNotInitialized, got %v", err)
	}
	if err := s.SaveConfig(DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	a := &App{Name: "web", Repo: "me/web", Branch: "main"}
	if err := s.SaveApp(a); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		s.SaveRelease("web", &Release{N: n, Status: StatusRunning})
	}
	if got := s.NextReleaseNumber("web"); got != 4 {
		t.Fatalf("next release %d", got)
	}
	rs, _ := s.Releases("web")
	if len(rs) != 3 || rs[0].N != 3 {
		t.Fatalf("releases not newest-first: %+v", rs)
	}
	s.SetCurrent("web", 2)
	if s.Current("web") != 2 || s.CurrentRelease("web").N != 2 {
		t.Fatal("current")
	}
	// env files are private
	s.SaveEnv("web", Env{"K": "v"})
	fi, _ := os.Stat(s.Path("apps", "web", "env"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("env mode %v", fi.Mode())
	}
	// no temp files left behind
	entries, _ := os.ReadDir(s.AppDir("web"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestCloneURL(t *testing.T) {
	cases := map[string]string{
		"me/app":              "https://github.com/me/app.git",
		"/tmp/repo":           "/tmp/repo",
		"https://x.org/r.git": "https://x.org/r.git",
	}
	for repo, want := range cases {
		if got := (&App{Repo: repo}).CloneURL(); got != want {
			t.Errorf("%s → %s, want %s", repo, got, want)
		}
	}
	if got := (&App{Repo: "me/app", DeployKey: true}).CloneURL(); got != "git@github.com:me/app.git" {
		t.Error(got)
	}
}
