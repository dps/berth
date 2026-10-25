// Package launchd writes and loads Berth's three LaunchAgents
// (dev.berth.berthd, dev.berth.caddy, dev.berth.cloudflared). They run in
// the user's session because Apple's container service is per-user.
package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/dps/berth/templates"
)

type Agent struct {
	Label string
	Args  []string
	Env   map[string]string
	Log   string
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Plist renders the agent's property list.
func Plist(a Agent) (string, error) {
	src, err := templates.FS.ReadFile("launchd.plist")
	if err != nil {
		return "", err
	}
	t, err := template.New("plist").Parse(string(src))
	if err != nil {
		return "", err
	}
	esc := Agent{Label: xmlEscape(a.Label), Log: xmlEscape(a.Log), Env: map[string]string{}}
	for _, arg := range a.Args {
		esc.Args = append(esc.Args, xmlEscape(arg))
	}
	for k, v := range a.Env {
		esc.Env[xmlEscape(k)] = xmlEscape(v)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, esc); err != nil {
		return "", err
	}
	return b.String(), nil
}

func AgentsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

func PlistPath(label string) string { return filepath.Join(AgentsDir(), label+".plist") }

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Install writes the plist and (re)loads it.
func Install(a Agent) error {
	body, err := Plist(a)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(AgentsDir(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.Log), 0o700); err != nil {
		return err
	}
	path := PlistPath(a.Label)
	old, _ := os.ReadFile(path)
	if string(old) == body && Loaded(a.Label) {
		return nil
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	if Loaded(a.Label) {
		launchctl("bootout", domain()+"/"+a.Label)
		// bootout returns before the job is fully gone; bootstrapping too
		// soon fails with "Bootstrap failed: 5: Input/output error".
		for i := 0; i < 50 && Loaded(a.Label); i++ {
			time.Sleep(200 * time.Millisecond)
		}
	}
	for attempt := 0; ; attempt++ {
		_, err = launchctl("bootstrap", domain(), path)
		if err == nil || attempt == 4 || Loaded(a.Label) {
			return err
		}
		time.Sleep(time.Second)
	}
}

// Uninstall unloads and removes the agent.
func Uninstall(label string) error {
	if Loaded(label) {
		launchctl("bootout", domain()+"/"+label)
	}
	err := os.Remove(PlistPath(label))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Loaded reports whether launchd knows the agent.
func Loaded(label string) bool {
	_, err := launchctl("print", domain()+"/"+label)
	return err == nil
}

// Running reports whether the agent has a live process.
func Running(label string) bool {
	out, err := launchctl("print", domain()+"/"+label)
	return err == nil && strings.Contains(out, "state = running")
}

// Restart kills and restarts the agent's process.
func Restart(label string) error {
	_, err := launchctl("kickstart", "-k", domain()+"/"+label)
	return err
}

const (
	LabelBerthd      = "dev.berth.berthd"
	LabelCaddy       = "dev.berth.caddy"
	LabelCloudflared = "dev.berth.cloudflared"
)
