// Package doctor checks every assumption Berth makes about the Mac and
// prints a fix for each failure.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dps/berth/internal/caddy"
	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/launchd"
	"github.com/dps/berth/internal/rpc"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

type Level string

const (
	OK   Level = "ok"
	Warn Level = "warn"
	Fail Level = "fail"
	Skip Level = "skip"
)

type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

type Doctor struct {
	Store *state.Store
	RT    runtime.Runtime
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// MacOSMajor returns the major macOS version (26 for macOS 26.x).
func MacOSMajor() int {
	out, err := run("sw_vers", "-productVersion")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.Split(out, ".")[0])
	return n
}

func (d *Doctor) Run(ctx context.Context, images bool) []Check {
	var cs []Check
	add := func(c Check) { cs = append(cs, c) }

	// Platform.
	if goruntime.GOARCH == "arm64" {
		add(Check{Name: "Apple Silicon", Level: OK})
	} else {
		add(Check{Name: "Apple Silicon", Level: Fail, Detail: goruntime.GOARCH, Fix: "Berth needs an Apple Silicon Mac"})
	}
	if v := MacOSMajor(); v >= 26 {
		add(Check{Name: "macOS 26 or later", Level: OK, Detail: fmt.Sprintf("macOS %d", v)})
	} else {
		add(Check{Name: "macOS 26 or later", Level: Fail, Detail: fmt.Sprintf("macOS %d", v), Fix: "update macOS in System Settings → General → Software Update"})
	}

	// Tools.
	for _, tool := range []struct{ name, fix string }{
		{"git", "xcode-select --install"},
		{"container", "brew install container"},
		{"caddy", "brew install caddy"},
	} {
		p := runtime.LookBin(tool.name)
		if _, err := os.Stat(p); err != nil {
			add(Check{Name: tool.name + " installed", Level: Fail, Fix: tool.fix})
		} else {
			add(Check{Name: tool.name + " installed", Level: OK, Detail: p})
		}
	}

	cfg, err := d.Store.Config()
	if err != nil {
		add(Check{Name: "berth initialized", Level: Fail, Detail: err.Error(), Fix: "berth init --domain example.com"})
		return cs
	}
	add(Check{Name: "berth initialized", Level: OK, Detail: d.Store.Root})

	if !cfg.NoTunnel {
		if p := runtime.LookBin("cloudflared"); p == "cloudflared" {
			add(Check{Name: "cloudflared installed", Level: Fail, Fix: "brew install cloudflared"})
		} else {
			add(Check{Name: "cloudflared installed", Level: OK})
		}
	}

	// container.
	if v, err := d.RT.Version(ctx); err == nil {
		lvl, detail := OK, v
		if cfg.ContainerTested != "" && v != cfg.ContainerTested {
			lvl, detail = Warn, fmt.Sprintf("%s (Berth is tested against %s)", v, cfg.ContainerTested)
		}
		add(Check{Name: "container version", Level: lvl, Detail: detail})
	}
	if d.RT.SystemRunning(ctx) {
		add(Check{Name: "container system running", Level: OK})
		if gw, err := d.RT.EnsureNetwork(ctx, cfg.Network); err == nil {
			add(Check{Name: "berth network", Level: OK, Detail: "gateway " + gw})
			add(d.outboundCheck(ctx, cfg))
		} else {
			add(Check{Name: "berth network", Level: Fail, Detail: err.Error(), Fix: "container network create " + cfg.Network})
		}
	} else {
		add(Check{Name: "container system running", Level: Fail, Fix: "container system start"})
	}

	// launchd.
	if cfg.Launchd {
		for _, l := range []string{launchd.LabelBerthd, launchd.LabelCaddy, launchd.LabelCloudflared} {
			if l == launchd.LabelCloudflared && (cfg.NoTunnel || cfg.TunnelID == "") {
				continue
			}
			switch {
			case launchd.Running(l):
				add(Check{Name: "launchd " + l, Level: OK})
			case launchd.Loaded(l):
				add(Check{Name: "launchd " + l, Level: Fail, Detail: "loaded but not running", Fix: fmt.Sprintf("launchctl kickstart -k gui/%d/%s; see %s", os.Getuid(), l, d.Store.Path("logs"))})
			default:
				add(Check{Name: "launchd " + l, Level: Fail, Detail: "not loaded", Fix: "berth init (re-run; it is idempotent)"})
			}
		}
	} else {
		add(Check{Name: "launchd agents", Level: Warn, Detail: "not installed (berthd runs in the foreground)", Fix: "berth init without --no-launchd"})
	}

	// berthd.
	if err := rpc.NewClient(d.Store.SocketPath()).Ping(ctx); err == nil {
		add(Check{Name: "berthd answering", Level: OK})
	} else {
		add(Check{Name: "berthd answering", Level: Fail, Detail: err.Error(), Fix: fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), launchd.LabelBerthd)})
	}
	if resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + cfg.WebhookAddr + "/healthz"); err == nil {
		resp.Body.Close()
		add(Check{Name: "webhook listener", Level: OK, Detail: cfg.WebhookAddr})
	} else {
		add(Check{Name: "webhook listener", Level: Fail, Detail: err.Error(), Fix: "check berthd's log: " + d.Store.Path("logs", "berthd.log")})
	}

	// Caddy.
	if err := caddy.NewClient(cfg.CaddyAdmin).Ping(ctx); err == nil {
		add(Check{Name: "Caddy admin API", Level: OK, Detail: cfg.CaddyAdmin})
	} else {
		add(Check{Name: "Caddy admin API", Level: Fail, Detail: err.Error(), Fix: fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), launchd.LabelCaddy)})
	}

	// Tunnel and DNS.
	if cfg.NoTunnel {
		add(Check{Name: "Cloudflare tunnel", Level: Skip, Detail: "--no-tunnel"})
	} else {
		if tunnel.Ready(ctx, cfg.TunnelMetrics) {
			add(Check{Name: "Cloudflare tunnel connected", Level: OK})
			if v, err := tunnel.RunningConfigVersion(ctx, cfg.TunnelMetrics); err == nil {
				switch {
				case cfg.TunnelRemote && v < 1:
					add(Check{Name: "tunnel ingress", Level: Fail, Detail: "cloudflared is using its local config, so custom domains won't update", Fix: "berth init (reinstalls cloudflared with the tunnel token)"})
				case cfg.TunnelRemote:
					add(Check{Name: "tunnel ingress", Level: OK, Detail: fmt.Sprintf("managed remotely (version %d)", v)})
				default:
					add(Check{Name: "tunnel ingress", Level: OK, Detail: "local config file"})
				}
			}
		} else {
			add(Check{Name: "Cloudflare tunnel connected", Level: Fail, Fix: fmt.Sprintf("launchctl kickstart -k gui/%d/%s; logs in %s", os.Getuid(), launchd.LabelCloudflared, d.Store.Path("logs"))})
		}
		probe := "berth-doctor." + cfg.Domain
		if addrs, err := net.DefaultResolver.LookupHost(ctx, probe); err == nil && len(addrs) > 0 {
			add(Check{Name: "wildcard DNS", Level: OK, Detail: probe + " → " + addrs[0]})
		} else {
			add(Check{Name: "wildcard DNS", Level: Fail, Detail: fmt.Sprint(err), Fix: fmt.Sprintf("cloudflared tunnel route dns %s '*.%s'", cfg.TunnelName, cfg.Domain)})
		}
		certDir := filepath.Join(d.Store.SecretsDir(), "cloudflared", "zones")
		if oc, err := tunnel.ReadOriginCert(tunnel.ZoneCertPath(certDir, cfg.Domain)); err != nil {
			add(Check{Name: "Cloudflare login", Level: Fail, Detail: "no login for " + cfg.Domain, Fix: "berth init (opens the browser)"})
		} else if err := tunnel.NewAPI(oc.APIToken).Verify(ctx); err != nil {
			add(Check{Name: "Cloudflare login", Level: Fail, Detail: err.Error(), Fix: "rm " + tunnel.ZoneCertPath(certDir, cfg.Domain) + " && berth init"})
		} else {
			zones := []string{}
			entries, _ := os.ReadDir(certDir)
			for _, e := range entries {
				zones = append(zones, strings.TrimSuffix(e.Name(), ".pem"))
			}
			add(Check{Name: "Cloudflare login", Level: OK, Detail: "zones " + strings.Join(zones, ", ")})
		}
		if tok, _ := os.ReadFile(filepath.Join(d.Store.SecretsDir(), "cloudflare.token")); len(tok) > 0 {
			if err := tunnel.NewAPI(strings.TrimSpace(string(tok))).Verify(ctx); err != nil {
				add(Check{Name: "Cloudflare API token", Level: Fail, Detail: err.Error(), Fix: "berth init --cf-token <new token>"})
			} else {
				add(Check{Name: "Cloudflare API token", Level: OK})
			}
		}
	}

	// GitHub.
	if tok, _ := os.ReadFile(filepath.Join(d.Store.SecretsDir(), "github.token")); len(tok) > 0 {
		if login, err := github.New(strings.TrimSpace(string(tok))).User(ctx); err == nil {
			add(Check{Name: "GitHub token", Level: OK, Detail: "@" + login})
		} else {
			add(Check{Name: "GitHub token", Level: Fail, Detail: err.Error(), Fix: "berth init --github-token <token>"})
		}
	} else {
		add(Check{Name: "GitHub token", Level: Warn, Detail: "none: only public repos, no webhooks", Fix: "berth init --github-token <token>"})
	}

	// Disk.
	if free, err := deploy.FreeBytes(d.Store.Root); err == nil {
		lvl := OK
		if free < 2<<30 {
			lvl = Fail
		} else if free < 10<<30 {
			lvl = Warn
		}
		add(Check{Name: "free disk", Level: lvl, Detail: deploy.HumanBytes(int64(free)), Fix: fixIf(lvl != OK, "berth gc")})
	}

	// Power and reboot behaviour.
	add(sleepCheck())
	add(fileVaultCheck(cfg))

	if images {
		cs = append(cs, d.imageChecks(ctx)...)
	}
	return cs
}

// outboundCheck starts a throwaway Alpine container on the berth network
// and fetches an HTTPS URL, which is what builds and apps need.
func (d *Doctor) outboundCheck(ctx context.Context, cfg *state.Config) Check {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out strings.Builder
	err := d.RT.RunWith(ctx, runtime.RunOpts{
		Image:   "alpine:3",
		Network: cfg.Network,
		Remove:  true,
		Args:    []string{"wget", "-q", "-T", "8", "-O", "/dev/null", "https://cloudflare.com/cdn-cgi/trace"},
	}, nil, &out)
	if err != nil {
		return Check{Name: "containers reach the internet", Level: Fail, Detail: lastLine(err.Error()),
			Fix: "container system stop && container system start (running apps restart automatically)"}
	}
	return Check{Name: "containers reach the internet", Level: OK}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func fixIf(cond bool, fix string) string {
	if cond {
		return fix
	}
	return ""
}

func sleepCheck() Check {
	out, err := run("pmset", "-g")
	if err != nil {
		return Check{Name: "sleep settings", Level: Skip, Detail: err.Error()}
	}
	vals := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			vals[f[0]] = f[1]
		}
	}
	var bad []string
	if vals["sleep"] != "0" {
		bad = append(bad, "sleep="+vals["sleep"])
	}
	if v, ok := vals["autorestart"]; ok && v != "1" {
		bad = append(bad, "autorestart="+v)
	}
	if len(bad) == 0 {
		return Check{Name: "sleep settings", Level: OK, Detail: "never sleeps, restarts after power loss"}
	}
	return Check{Name: "sleep settings", Level: Warn, Detail: strings.Join(bad, " "), Fix: "sudo pmset -a sleep 0 disksleep 0 womp 1 autorestart 1"}
}

func fileVaultCheck(cfg *state.Config) Check {
	out, _ := run("fdesetup", "status")
	on := strings.Contains(out, "FileVault is On")
	autoLogin, _ := run("defaults", "read", "/Library/Preferences/com.apple.loginwindow", "autoLoginUser")
	switch {
	case cfg.Unattended && on:
		return Check{Name: "reboot mode", Level: Fail, Detail: "--unattended, but FileVault is on (auto-login is impossible)", Fix: "System Settings → Privacy & Security → FileVault → Turn Off, then Users & Groups → Automatic login"}
	case cfg.Unattended && autoLogin == "":
		return Check{Name: "reboot mode", Level: Warn, Detail: "--unattended, but automatic login is off", Fix: "System Settings → Users & Groups → Automatically log in as …"}
	case cfg.Unattended:
		return Check{Name: "reboot mode", Level: OK, Detail: "unattended: survives power cuts (disk not encrypted)"}
	case on:
		return Check{Name: "reboot mode", Level: Warn, Detail: "FileVault on: a power cut needs someone to type the password; use `berth reboot` for planned restarts", Fix: "a small UPS, or `berth init --unattended`"}
	default:
		return Check{Name: "reboot mode", Level: Warn, Detail: "FileVault off but not in unattended mode", Fix: "enable FileVault, or re-run `berth init --unattended` and turn on automatic login"}
	}
}

// imageChecks compares local base images with Docker Hub's current digest.
func (d *Doctor) imageChecks(ctx context.Context) []Check {
	imgs, err := d.RT.Images(ctx)
	if err != nil {
		return []Check{{Name: "images", Level: Skip, Detail: err.Error()}}
	}
	var cs []Check
	client := &http.Client{Timeout: 10 * time.Second}
	for _, img := range imgs {
		if strings.HasPrefix(img.Name, "berth/") || strings.Contains(strings.Split(img.Name, "/")[0], ".") {
			continue // our builds, or another registry
		}
		repo, tag, ok := strings.Cut(img.Name, ":")
		if !ok {
			tag = "latest"
		}
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
		remote, err := hubDigest(ctx, client, repo, tag)
		if err != nil {
			cs = append(cs, Check{Name: "image " + img.Name, Level: Skip, Detail: err.Error()})
			continue
		}
		if remote == img.Digest || "sha256:"+img.Digest == remote {
			cs = append(cs, Check{Name: "image " + img.Name, Level: OK, Detail: "up to date"})
		} else {
			cs = append(cs, Check{Name: "image " + img.Name, Level: Warn, Detail: "newer version available", Fix: "container image pull " + img.Name + " (then redeploy apps that use it)"})
		}
	}
	return cs
}

func hubDigest(ctx context.Context, c *http.Client, repo, tag string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://auth.docker.io/token?service=registry.docker.io&scope=repository:"+repo+":pull", nil)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	req, _ = http.NewRequestWithContext(ctx, "HEAD", "https://registry-1.docker.io/v2/"+repo+"/manifests/"+tag, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err = c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("registry: %s", resp.Status)
	}
	return resp.Header.Get("Docker-Content-Digest"), nil
}
