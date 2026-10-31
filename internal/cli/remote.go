package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/state"
)

// ---- using Berth from another machine (design.md, Working from another machine) ----
//
// On a machine without Berth (a laptop), commands run on the Berth Mac over
// SSH. The Mac is $BERTH_HOST, or the host in this repo's `berth` git remote.

// sshTarget is the Berth Mac as ssh reaches it.
type sshTarget struct {
	dst  string // host, or user@host
	port string
}

func (t *sshTarget) hostname() string {
	_, h, ok := strings.Cut(t.dst, "@")
	if !ok {
		return t.dst
	}
	return h
}

func (t *sshTarget) String() string {
	if t.port != "" {
		return t.dst + " (port " + t.port + ")"
	}
	return t.dst
}

// parseSSHTarget reads "host", "user@host" or "ssh://user@host:port/...".
func parseSSHTarget(s string) (*sshTarget, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		if s == "" || strings.ContainsAny(s, " /'\"") || strings.HasPrefix(s, "-") {
			return nil, fmt.Errorf("%q isn't an SSH host", s)
		}
		return &sshTarget{dst: s}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" {
		return nil, fmt.Errorf("%q isn't an ssh:// URL", s)
	}
	t := &sshTarget{dst: u.Hostname(), port: u.Port()}
	if u.User != nil && u.User.Username() != "" {
		t.dst = u.User.Username() + "@" + t.dst
	}
	if strings.HasPrefix(t.dst, "-") || strings.ContainsAny(t.dst, " '\"") {
		return nil, fmt.Errorf("%q isn't an SSH host", s)
	}
	return t, nil
}

var errNoBerth = hintErr(errors.New("Berth isn't set up on this machine"),
	"run this on the Berth Mac, or tell the CLI where it is: export BERTH_HOST=you@your-mac.local (any name `ssh` reaches it by). Inside an app's repo, its `berth` git remote does the same.")

// isBerthMac reports whether this machine runs Berth.
func isBerthMac() bool {
	_, err := store().Config()
	return !errors.Is(err, state.ErrNotInitialized)
}

// berthTarget finds the Berth Mac from this machine.
func berthTarget() (*sshTarget, error) {
	if h := os.Getenv("BERTH_HOST"); h != "" {
		t, err := parseSSHTarget(h)
		if err != nil {
			return nil, fmt.Errorf("BERTH_HOST: %w", err)
		}
		return t, nil
	}
	if raw, err := gitIn(".", nil, "remote", "get-url", "berth"); err == nil {
		return parseSSHTarget(raw)
	}
	return nil, errNoBerth
}

// command builds `ssh <mac> berth <args>`. With tty, ssh allocates a
// terminal, for prompts, shells and colour.
func (t *sshTarget) command(args []string, tty bool) *exec.Cmd {
	remote := "PATH=/opt/homebrew/bin:/usr/local/bin:$PATH BERTH_CLIENT_VERSION=" + shellQuote(Version) + " berth"
	for _, a := range args {
		remote += " " + shellQuote(a)
	}
	sshArgs := []string{"-o", "LogLevel=ERROR"}
	if tty {
		sshArgs = append(sshArgs, "-t")
	}
	if t.port != "" {
		sshArgs = append(sshArgs, "-p", t.port)
	}
	return exec.Command("ssh", append(sshArgs, "--", t.dst, remote)...)
}

// output runs a berth command on the Mac and returns its stdout; its
// stderr (progress, errors) comes through.
func (t *sshTarget) output(args ...string) ([]byte, error) {
	c := t.command(args, false)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, os.Stderr
	if err := c.Run(); err != nil {
		return nil, sshError(t, err)
	}
	return out.Bytes(), nil
}

// run runs a berth command on the Mac with this terminal.
func (t *sshTarget) run(args ...string) error {
	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	c := t.command(args, tty)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return sshError(t, err)
	}
	return nil
}

func sshError(t *sshTarget, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == 255 {
			return hintErr(fmt.Errorf("couldn't reach %s over SSH", t),
				fmt.Sprintf("check that `ssh %s` works, and that Remote Login is on for the Mac (System Settings → General → Sharing)", t.dst))
		}
		// The remote berth has already printed its error.
		return &silentExit{code: ee.ExitCode()}
	}
	return err
}

// localOnly commands run on this machine even when Berth isn't here: they
// work on local files, or reach the Mac themselves.
var localOnly = map[string]bool{
	"new": true, "dev:push": true, "git:remote": true, "admin": true, "upgrade": true,
	"init": true, "uninstall": true, "trust-ca": true,
	"help": true, "completion": true, cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true,
}

// forward runs the command line on the Berth Mac when this machine isn't
// one. It reports whether it did, and the exit code.
func forward(root *cobra.Command, args []string) (int, bool) {
	if isBerthMac() {
		return 0, false
	}
	cmd, rest, err := root.Find(args)
	if err != nil || cmd == root || localOnly[cmd.Name()] {
		return 0, false
	}
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return 0, false
		}
	}
	t, err := berthTarget()
	if err != nil {
		return printErr(err), true
	}
	c := t.command(args, term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())))
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	// config:import reads a file: send this machine's copy.
	if cmd.Name() == "config:import" {
		if file := positional(rest, 1); file != "" && file != "-" {
			f, err := os.Open(file)
			if err != nil {
				return printErr(err), true
			}
			defer f.Close()
			fwd := append([]string{}, args...)
			for i := len(fwd) - 1; i >= 0; i-- {
				if fwd[i] == file {
					fwd[i] = "-"
					break
				}
			}
			c = t.command(fwd, false)
			c.Stdin, c.Stdout, c.Stderr = f, os.Stdout, os.Stderr
		}
	}
	if err := c.Run(); err != nil {
		if err = sshError(t, err); err != nil {
			var silent *silentExit
			if errors.As(err, &silent) {
				return silent.code, true
			}
			return printErr(err), true
		}
	}
	return 0, true
}

// positional returns the i'th non-flag argument.
func positional(args []string, i int) string {
	n := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		if n == i {
			return a
		}
		n++
	}
	return ""
}

// ---- berth info: what another machine needs to know about this Berth ----

// BerthInfo describes a Berth Mac to `berth new` on any machine.
type BerthInfo struct {
	Version  string `json:"version"`
	Domain   string `json:"domain,omitempty"`
	Hostname string `json:"hostname"`
	SignIn   bool   `json:"signin"`
	AI       bool   `json:"ai"`
	Tools    bool   `json:"tools"`
}

func localInfo() (*BerthInfo, error) {
	cfg, err := store().Config()
	if err != nil {
		return nil, err
	}
	info := &BerthInfo{Version: Version, Domain: cfg.Domain, Hostname: cfg.Hostname, AI: store().AIConfigured(), Tools: store().ToolsConfigured()}
	if ac, err := store().AuthConfig(); err == nil && ac.Provider != "" {
		info.SignIn = true
	}
	return info, nil
}

func cmdInfo() *cobra.Command {
	return &cobra.Command{
		Use:    "info",
		Short:  "Describe this Berth as JSON (for berth new on another machine)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := localInfo()
			if err != nil {
				return err
			}
			printJSON(info)
			return nil
		},
	}
}

func remoteInfo(t *sshTarget) (*BerthInfo, error) {
	out, err := t.output("info")
	if err != nil {
		return nil, err
	}
	var info BerthInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("unexpected reply from %s: %s", t, strings.TrimSpace(string(out)))
	}
	return &info, nil
}
