package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ---- versions and upgrades (design.md, Releases and upgrades) ----

// releaseRepo is where releases are tagged.
const releaseRepo = "dps/berth"

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// parseVersion reads a release version ("v0.2.0" or Homebrew's "0.2.0").
// Anything else (dev builds, HEAD-abc1234, v0.2.0-3-gabc1234) isn't one.
func parseVersion(v string) ([3]int, bool) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// compareVersions is -1, 0 or 1 for two release versions; ok is false
// when either isn't one.
func compareVersions(a, b string) (cmp int, ok bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va {
		switch {
		case va[i] < vb[i]:
			return -1, true
		case va[i] > vb[i]:
			return 1, true
		}
	}
	return 0, true
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// versionSkew is the warning a Berth Mac prints when a laptop's CLI runs a
// command on it with a different version (BERTH_CLIENT_VERSION over SSH).
func versionSkew(client, here string) string {
	if client == "" || here == "dev" || client == "dev" || sameVersion(client, here) {
		return ""
	}
	switch c, ok := compareVersions(client, here); {
	case ok && c > 0:
		return fmt.Sprintf("this Mac runs Berth %s, older than your CLI (%s): upgrade it with `berth upgrade`", here, client)
	case ok && c < 0:
		return fmt.Sprintf("your CLI (%s) is older than this Mac's Berth (%s): run `berth upgrade` where you ran the command", client, here)
	}
	return fmt.Sprintf("your CLI (%s) and this Mac's Berth (%s) are different builds; `berth upgrade` on both brings them in line", client, here)
}

// latestRelease asks GitHub for the highest vX.Y.Z tag.
func latestRelease(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+releaseRepo+"/tags?per_page=100", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", err
	}
	best := ""
	for _, t := range tags {
		if _, ok := parseVersion(t.Name); !ok {
			continue
		}
		if c, _ := compareVersions(t.Name, best); best == "" || c > 0 {
			best = t.Name
		}
	}
	if best == "" {
		return "", fmt.Errorf("no releases yet")
	}
	return best, nil
}

// installMethod says how this berth was installed and the commands that
// upgrade it.
func installMethod() (how string, upgrade [][]string) {
	exe, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
	}
	if i := strings.Index(exe, "/Cellar/berth/"); i >= 0 {
		// A --HEAD install lives in Cellar/berth/HEAD-<sha>, and plain
		// `brew upgrade` skips those.
		if strings.HasPrefix(exe[i+len("/Cellar/berth/"):], "HEAD-") {
			return "Homebrew (--HEAD)", [][]string{{"brew", "upgrade", "--fetch-HEAD", "berth"}}
		}
		// Homebrew refreshes its own formulas by itself, but a third-party
		// tap's only on `brew update`: without it, upgrade sees no new version.
		return "Homebrew", [][]string{{"brew", "update"}, {"brew", "upgrade", "berth"}}
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path == "github.com/dps/berth" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		// `go install …@version` builds from the module cache; a build in a
		// checkout records its git revision instead.
		local := false
		for _, kv := range bi.Settings {
			local = local || kv.Key == "vcs.revision"
		}
		if !local {
			return "go install", [][]string{{"go", "install", "github.com/dps/berth/cmd/berth@latest"}}
		}
	}
	return "a source build", nil
}

func cmdUpgrade() *cobra.Command {
	var check, yes bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Berth here, and from another machine on the Berth Mac too",
		Long: "Upgrades Berth the way it was installed (Homebrew or go install). On the Berth Mac, berthd\n" +
			"restarts itself on the new version within seconds and apps keep running. From another\n" +
			"machine it upgrades this CLI, then offers to upgrade the Mac.",
		Example: "  berth upgrade --check\n  berth upgrade",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			how, up := installMethod()
			latest, lerr := latestRelease(ctx)
			fmt.Printf("This machine: berth %s, installed with %s\n", Version, how)
			if lerr != nil {
				fmt.Printf("Latest release: unknown (%v)\n", lerr)
			} else {
				fmt.Printf("Latest release: %s\n", latest)
			}
			if check {
				return nil
			}
			if up == nil {
				return hintErr(fmt.Errorf("this berth was built from source, so Berth can't upgrade it"),
					"pull the latest code and run `make install`, or install with Homebrew as the README shows")
			}
			if c, ok := compareVersions(Version, latest); ok && c >= 0 && lerr == nil {
				fmt.Println("-----> Already up to date here")
			} else {
				for _, step := range up {
					fmt.Printf("-----> %s\n", strings.Join(step, " "))
					x := exec.Command(step[0], step[1:]...)
					x.Stdin, x.Stdout, x.Stderr = os.Stdin, os.Stdout, os.Stderr
					if err := x.Run(); err != nil {
						return fmt.Errorf("%s: %w", strings.Join(step, " "), err)
					}
				}
				// Check it took, rather than report success on faith.
				if now := installedVersion(); lerr == nil && now != "" && !sameVersion(now, latest) {
					return hintErr(fmt.Errorf("the upgrade finished, but berth is still %s, not %s", now, latest),
						fmt.Sprintf("try `%s` by hand", joinSteps(up)))
				}
			}
			if isBerthMac() {
				return waitForDaemonUpgrade()
			}
			t, err := berthTarget()
			if err != nil {
				return nil // a laptop with no Mac configured: just the CLI
			}
			if !yes && !askYes(fmt.Sprintf("Upgrade Berth on %s too?", t)) {
				return nil
			}
			if err := t.run("upgrade"); err != nil {
				return hintErr(err, "if the Mac's Berth predates `berth upgrade`, run `brew update && brew upgrade berth` there once")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only show the installed and latest versions")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "also upgrade the Berth Mac without asking")
	return c
}

// installedVersion asks the berth now on this path for its version (the
// one running is still the old one).
func installedVersion() string {
	exe, err := exec.LookPath("berth") // the link Homebrew repoints
	if err != nil {
		if exe, err = os.Executable(); err != nil {
			return ""
		}
	}
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1] // "berth version 0.1.1"
}

func joinSteps(steps [][]string) string {
	var parts []string
	for _, s := range steps {
		parts = append(parts, strings.Join(s, " "))
	}
	return strings.Join(parts, " && ")
}

// waitForDaemonUpgrade waits for berthd to restart on the new binary,
// which it does by itself once no deploy is running.
func waitForDaemonUpgrade() error {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	out, err := exec.Command(filepath.Join(filepath.Dir(exe), "berthd"), "-version").Output()
	if err != nil {
		return nil
	}
	want := strings.TrimSpace(string(out))
	fmt.Printf("-----> Waiting for berthd to restart on %s (apps keep running)\n", want)
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		var v struct {
			Version string `json:"version"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := client().Call(ctx, "ping", nil, nil, &v)
		cancel()
		if err == nil && sameVersion(v.Version, want) {
			fmt.Printf("-----> berthd is running %s\n", v.Version)
			return nil
		}
	}
	return hintErr(fmt.Errorf("berthd hasn't restarted on %s yet", want),
		"it waits for running deploys to finish; `berth status` shows its version, and `berth doctor` checks it")
}
