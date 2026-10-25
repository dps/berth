package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/caddy"
	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/doctor"
	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/launchd"
	"github.com/dps/berth/internal/rpc"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

type initOpts struct {
	domain      string
	noTunnel    bool
	tailscale   bool
	cfIPFilter  bool
	unattended  bool
	githubToken string
	cfToken     string
	noLaunchd   bool
	yes         bool
	tunnelName  string
	skipGitHub  bool
}

func step(format string, args ...any) { fmt.Printf("-----> "+format+"\n", args...) }
func note(format string, args ...any) { fmt.Printf("       "+format+"\n", args...) }

func ask(prompt string) string {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func cmdInit() *cobra.Command {
	var o initOpts
	c := &cobra.Command{
		Use:   "init --domain example.com",
		Short: "One-time setup: tunnel, Caddy, launchd agents, GitHub login (safe to re-run)",
		Example: "  berth init --domain example.com\n" +
			"  berth init --no-tunnel                       # LAN only\n" +
			"  berth init --domain example.com --github-token ghp_... --cf-token ... --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runInit(o) },
	}
	f := c.Flags()
	f.StringVar(&o.domain, "domain", "", "domain on your Cloudflare account; apps live at <app>.<domain>")
	f.BoolVar(&o.noTunnel, "no-tunnel", false, "no public ingress: LAN hostnames only")
	f.BoolVar(&o.tailscale, "tailscale", false, "also serve apps to your tailnet with Tailscale Services")
	f.BoolVar(&o.cfIPFilter, "cf-ip-filter", false, "only let GitHub's webhook IPs reach hooks.<domain> (Cloudflare WAF rule)")
	f.BoolVar(&o.unattended, "unattended", false, "plan for FileVault off + automatic login, so power cuts recover with no one present")
	f.StringVar(&o.githubToken, "github-token", "", "GitHub token with repo and admin:repo_hook scopes")
	f.StringVar(&o.cfToken, "cf-token", "", "Cloudflare API token with Zone · WAF · Edit, only needed for --cf-ip-filter")
	f.BoolVar(&o.noLaunchd, "no-launchd", false, "do not install LaunchAgents (run berthd yourself)")
	f.BoolVarP(&o.yes, "yes", "y", false, "non-interactive: accept defaults, skip optional prompts and sudo steps")
	f.StringVar(&o.tunnelName, "tunnel-name", "berth", "Cloudflare tunnel name")
	f.BoolVar(&o.skipGitHub, "skip-github", false, "no GitHub token (public repos only, polling)")
	return c
}

func runInit(o initOpts) error {
	ctx := context.Background()
	s := store()

	// 1. Platform and tools.
	step("Checking this Mac")
	if goruntime.GOARCH != "arm64" {
		return fmt.Errorf("Berth needs an Apple Silicon Mac")
	}
	if v := doctor.MacOSMajor(); v < 26 {
		return fmt.Errorf("Berth needs macOS 26 or later (this is macOS %d)", v)
	}
	cfg, err := s.Config()
	fresh := err != nil
	if fresh {
		cfg = state.DefaultConfig()
	}
	if o.domain != "" {
		cfg.Domain = strings.ToLower(strings.TrimSuffix(o.domain, "."))
	}
	if o.noTunnel {
		cfg.NoTunnel = true
	}
	if cfg.Domain == "" && !cfg.NoTunnel {
		return fmt.Errorf("--domain is required (or --no-tunnel for LAN-only)")
	}
	cfg.Tailscale = cfg.Tailscale || o.tailscale
	cfg.CFIPFilter = cfg.CFIPFilter || o.cfIPFilter
	cfg.Unattended = cfg.Unattended || o.unattended
	if cfg.TunnelName == "" {
		cfg.TunnelName = o.tunnelName
	}
	tools := []string{"git", "container", "caddy"}
	if !cfg.NoTunnel {
		tools = append(tools, "cloudflared")
	}
	for _, t := range tools {
		if _, err := os.Stat(runtime.LookBin(t)); err == nil {
			continue
		}
		if t == "git" {
			return fmt.Errorf("git is missing: run `xcode-select --install`")
		}
		if !o.yes && !confirm(fmt.Sprintf("%s is not installed. Run `brew install %s`?", t, t)) {
			return fmt.Errorf("%s is required (brew install %s)", t, t)
		}
		c := exec.Command(runtime.LookBin("brew"), "install", t)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("brew install %s: %w", t, err)
		}
	}
	note("Apple Silicon, macOS %d, %s", doctor.MacOSMajor(), strings.Join(tools, ", "))

	// 2. container.
	rt := runtime.NewApple()
	if !rt.SystemRunning(ctx) {
		step("Starting Apple's container system (first run downloads a Linux kernel)")
		if err := rt.SystemStart(ctx); err != nil {
			return err
		}
	}
	for _, dir := range []string{"", "secrets", "secrets/cloudflared", "logs", "apps", "services", "backups", "data", "caddy"} {
		if err := os.MkdirAll(s.Path(dir), 0o700); err != nil {
			return err
		}
	}
	os.Chmod(s.SecretsDir(), 0o700)
	gw, err := rt.EnsureNetwork(ctx, cfg.Network)
	if err != nil {
		return err
	}
	cfg.Gateway = gw
	if v, err := rt.Version(ctx); err == nil {
		note("container %s, network %q (gateway %s)", v, cfg.Network, gw)
	}
	if err := s.SaveConfig(cfg); err != nil {
		return err
	}

	// 3. Cloudflare tunnel.
	if !cfg.NoTunnel {
		if err := initTunnel(ctx, s, cfg, o); err != nil {
			return err
		}
	}

	// 4. GitHub.
	if !o.skipGitHub {
		if err := initGitHub(ctx, s, cfg, o); err != nil {
			return err
		}
	}

	// 5. Tailscale.
	if cfg.Tailscale {
		out, err := exec.Command(runtime.LookBin("tailscale"), "status", "--json").Output()
		if err != nil {
			return fmt.Errorf("--tailscale: `tailscale status` failed (is Tailscale installed and up?): %v", err)
		}
		var ts struct {
			MagicDNSSuffix string `json:"MagicDNSSuffix"`
		}
		json.Unmarshal(out, &ts)
		cfg.TailnetDomain = strings.TrimSuffix(ts.MagicDNSSuffix, ".")
		note("tailnet %s: apps will be served as Tailscale Services <app>.%s", cfg.TailnetDomain, cfg.TailnetDomain)
	}
	if err := s.SaveConfig(cfg); err != nil {
		return err
	}

	// 6. Base configs so Caddy and cloudflared can start before berthd.
	if _, err := os.Stat(s.Path("caddy.json")); err != nil {
		base := caddy.Build(caddy.Options{Admin: cfg.CaddyAdmin, HTTPListen: cfg.CaddyHTTP, HTTPSListen: cfg.CaddyHTTPS, LANHTTP: cfg.CaddyLANHTTP, StorageDir: s.Path("caddy")})
		b, _ := json.MarshalIndent(base, "", "  ")
		state.WriteFile(s.Path("caddy.json"), b, 0o600)
	}
	if !cfg.NoTunnel {
		cred := filepath.Join(s.SecretsDir(), "cloudflared", cfg.TunnelID+".json")
		yml := tunnel.LocalConfig(cfg.TunnelID, cred, tunnel.Ingress(cfg.Domain, cfg.WebhookAddr, cfg.CaddyHTTP, nil))
		if _, err := os.Stat(s.Path("cloudflared.yml")); err != nil {
			state.WriteFile(s.Path("cloudflared.yml"), []byte(yml), 0o600)
		}
	}

	// 7. launchd.
	if o.noLaunchd {
		cfg.Launchd = false
		s.SaveConfig(cfg)
		step("Skipping launchd; run these yourself:")
		note("caddy run --config %s", s.Path("caddy.json"))
		if !cfg.NoTunnel {
			note("cloudflared tunnel --config %s --metrics %s run %s", s.Path("cloudflared.yml"), cfg.TunnelMetrics, cfg.TunnelID)
		}
		note("berthd")
	} else {
		if err := installAgents(s, cfg); err != nil {
			return err
		}
		cfg.Launchd = true
		s.SaveConfig(cfg)
		if err := waitForDaemon(30 * time.Second); err != nil {
			return hintErr(err, "see "+s.Path("logs", "berthd.log"))
		}
		note("berthd is up")
		if cfg.CFIPFilter || o.cfToken != "" {
			if _, err := client().Call(ctx, "tunnel.apply", nil, os.Stdout); err != nil {
				note("warning: %v", err)
			}
		}
	}

	// 8. Power settings (sudo, with consent).
	if !o.yes {
		pm := "sudo pmset -a sleep 0 disksleep 0 womp 1 autorestart 1"
		if confirm(fmt.Sprintf("Keep this Mac awake and restart it after power loss (`%s`)?", pm)) {
			c := exec.Command("sudo", "pmset", "-a", "sleep", "0", "disksleep", "0", "womp", "1", "autorestart", "1")
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				note("pmset failed: %v", err)
			}
		}
	}

	// 9. Reboot mode.
	fv := daemon.FileVaultStatus()
	step("Reboots and FileVault")
	switch {
	case cfg.Unattended:
		note("Unattended mode: apps come back after a power cut with no one present.")
		note("Finish in System Settings (Berth will not script disk-encryption changes):")
		if fv == "on" {
			note("  1. Privacy & Security → FileVault → Turn Off (the disk will not be encrypted)")
		}
		note("  2. Users & Groups → Automatically log in as: %s", os.Getenv("USER"))
	case fv == "on":
		note("FileVault is on (recommended). Crashes and restarts recover by themselves;")
		note("use `berth reboot` for planned reboots. After a power cut someone must type")
		note("the password at the Mac before apps return — a small UPS avoids that.")
	default:
		note("FileVault is off. For unattended power-cut recovery, re-run with --unattended")
		note("and turn on automatic login; otherwise consider turning FileVault on.")
	}

	// 10. Summary.
	fmt.Println()
	checks := (&doctor.Doctor{Store: s, RT: rt}).Run(ctx, false)
	printChecks(checks)
	fmt.Println()
	if !o.yes {
		if _, err := os.Stat(caRootPath()); err == nil && confirm("Trust Caddy's local CA so *.local URLs show a lock in Safari?") {
			if err := trustCA(); err != nil {
				note("trust failed: %v", err)
			}
		}
	}
	fmt.Println("Berth is ready. Next:")
	fmt.Println("  berth create myapp --repo you/myapp")
	return nil
}

func initTunnel(ctx context.Context, s *state.Store, cfg *state.Config, o initOpts) error {
	cli := &tunnel.CLI{Bin: runtime.LookBin("cloudflared")}
	oc, err := cfLogin(ctx, s, cli, cfg.Domain)
	if err != nil {
		return err
	}
	cfg.CFAccountID, cfg.CFZoneID = oc.AccountID, oc.ZoneID
	cli.Origin = tunnel.ZoneCertPath(zoneCertDir(s), cfg.Domain)

	id, err := cli.FindTunnel(ctx, cfg.TunnelName)
	if err != nil {
		return err
	}
	if id == "" {
		step("Creating tunnel %q", cfg.TunnelName)
		tmp := filepath.Join(s.SecretsDir(), "cloudflared", "new.json")
		id, err = cli.Create(ctx, cfg.TunnelName, tmp)
		if err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(s.SecretsDir(), "cloudflared", id+".json")); err != nil {
			return err
		}
	}
	cred := filepath.Join(s.SecretsDir(), "cloudflared", id+".json")
	if _, err := os.Stat(cred); err != nil {
		// The tunnel exists (e.g. created on another Mac); fetch its credentials.
		if err := cli.FetchCredentials(ctx, cfg.TunnelName, cred); err != nil {
			return fmt.Errorf("tunnel %q exists but its credentials are not on this Mac; delete it (`cloudflared tunnel delete %s`) or copy %s.json into %s", cfg.TunnelName, cfg.TunnelName, id, filepath.Dir(cred))
		}
	}
	os.Chmod(cred, 0o600)
	cfg.TunnelID = id
	step("Routing *.%s to tunnel %s", cfg.Domain, id[:8])
	if err := cli.RouteDNS(ctx, id, "*."+cfg.Domain); err != nil {
		return err
	}
	// Remote management: publish the ingress now, and run cloudflared with
	// the tunnel token so it follows later changes (custom domains) live.
	// Without that, cloudflared uses the local config file and is restarted
	// when it changes.
	cfg.TunnelRemote = false
	rules := tunnel.Ingress(cfg.Domain, cfg.WebhookAddr, cfg.CaddyHTTP, existingDomains(s))
	if err := tunnel.NewAPI(oc.APIToken).PutTunnelConfig(ctx, cfg.CFAccountID, id, rules); err != nil {
		note("remote tunnel configuration unavailable (%v); using a local config file", err)
	} else if tok, err := cli.Token(ctx, id); err != nil {
		note("could not get the tunnel token (%v); using a local config file", err)
	} else if err := state.WriteFile(tunnel.TokenPath(s.SecretsDir(), id), []byte(tok+"\n"), 0o600); err == nil {
		cfg.TunnelRemote = true
		note("tunnel ingress is managed remotely: domain changes apply live")
	}

	// A scoped API token is optional; the browser login covers everything
	// except the webhook IP filter (which needs Zone · WAF · Edit).
	tokPath := filepath.Join(s.SecretsDir(), "cloudflare.token")
	tok := o.cfToken
	if tok == "" && cfg.CFIPFilter {
		if b, err := os.ReadFile(tokPath); err == nil {
			tok = strings.TrimSpace(string(b))
		} else if !o.yes {
			note("--cf-ip-filter needs an API token with Zone · WAF · Edit for %s;", cfg.Domain)
			note("create one at https://dash.cloudflare.com/profile/api-tokens")
			tok = ask("       Paste it: ")
		}
		if tok == "" {
			return fmt.Errorf("--cf-ip-filter needs a Cloudflare API token (--cf-token) with Zone · WAF · Edit")
		}
	}
	if tok != "" {
		if err := tunnel.NewAPI(tok).Verify(ctx); err != nil {
			return fmt.Errorf("Cloudflare token: %w", err)
		}
		if err := state.WriteFile(tokPath, []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
		note("Cloudflare API token saved")
	}
	return nil
}

// existingDomains lists custom domains of existing apps (re-running init).
func existingDomains(s *state.Store) []string {
	apps, _ := s.Apps()
	var out []string
	for _, a := range apps {
		if !a.Private {
			out = append(out, a.Domains...)
		}
	}
	return out
}

func zoneCertDir(s *state.Store) string { return filepath.Join(s.SecretsDir(), "cloudflared", "zones") }

// cfLogin returns the saved login for zone, or opens the browser for a new
// one (cloudflared tunnel login) and checks the user picked that zone.
func cfLogin(ctx context.Context, s *state.Store, cli *tunnel.CLI, zone string) (*tunnel.OriginCert, error) {
	path := tunnel.ZoneCertPath(zoneCertDir(s), zone)
	if oc, err := tunnel.ReadOriginCert(path); err == nil {
		return oc, nil
	}
	step("Opening Cloudflare in your browser: log in and choose %s", zone)
	oc, err := cli.Login(ctx, path, os.Stdout)
	if err != nil {
		return nil, err
	}
	if z, err := tunnel.NewAPI(oc.APIToken).ZoneByID(ctx, oc.ZoneID); err == nil && z.Name != zone {
		os.Remove(path)
		return nil, fmt.Errorf("you authorized %s in the browser, not %s; run the command again and pick %s", z.Name, zone, zone)
	}
	note("Cloudflare: logged in for %s", zone)
	return oc, nil
}

func initGitHub(ctx context.Context, s *state.Store, cfg *state.Config, o initOpts) error {
	path := filepath.Join(s.SecretsDir(), "github.token")
	try := func(tok, source string) bool {
		if tok == "" {
			return false
		}
		login, err := github.New(tok).User(ctx)
		if err != nil {
			note("GitHub token from %s did not work: %v", source, err)
			return false
		}
		if err := state.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
			note("saving GitHub token: %v", err)
			return false
		}
		cfg.GitHubUser = login
		step("GitHub: signed in as @%s (%s)", login, source)
		return true
	}
	if try(o.githubToken, "--github-token") {
		return nil
	}
	if b, err := os.ReadFile(path); err == nil && try(strings.TrimSpace(string(b)), "saved login") {
		return nil
	}
	if clientID := github.ClientID(); clientID != "" && !o.yes {
		tok, err := githubBrowserLogin(ctx, clientID)
		if err != nil {
			note("GitHub browser login failed: %v", err)
		} else if try(tok, "browser login") {
			return nil
		}
	}
	if out, err := exec.Command(runtime.LookBin("gh"), "auth", "token").Output(); err == nil && try(strings.TrimSpace(string(out)), "gh auth token") {
		return nil
	}
	if !o.yes {
		note("GitHub: Berth needs a token to clone private repos and register webhooks.")
		note("Create a classic token with scopes `repo` and `admin:repo_hook` at")
		note("https://github.com/settings/tokens/new?scopes=repo,admin:repo_hook&description=berth")
		if try(ask("       Paste it (or press Enter to skip; public repos still work by polling): "), "pasted") {
			return nil
		}
	}
	note("No GitHub login: public repos only, deploys by polling")
	return nil
}

// githubBrowserLogin runs GitHub's device flow: the code is copied to the
// clipboard and the browser opens on the page that asks for it.
func githubBrowserLogin(ctx context.Context, clientID string) (string, error) {
	gh := github.New("")
	dc, err := gh.StartDeviceFlow(ctx, clientID, []string{"repo", "admin:repo_hook"})
	if err != nil {
		return "", err
	}
	copied := ""
	clip := exec.Command("pbcopy")
	clip.Stdin = strings.NewReader(dc.UserCode)
	if clip.Run() == nil {
		copied = " (copied to your clipboard)"
	}
	step("Opening GitHub in your browser to authorize Berth")
	note("Enter the code %s%s at %s", dc.UserCode, copied, dc.VerificationURI)
	exec.Command("open", dc.VerificationURI).Run()
	return gh.PollDeviceFlow(ctx, clientID, dc)
}

// installAgents writes and loads the three LaunchAgents.
func installAgents(s *state.Store, cfg *state.Config) error {
	berthd, err := berthdPath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	env := map[string]string{
		"PATH":       "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME":       home,
		"BERTH_HOME": s.Root,
	}
	agents := []launchd.Agent{
		{Label: launchd.LabelCaddy, Args: []string{runtime.LookBin("caddy"), "run", "--config", s.Path("caddy.json")}, Env: env, Log: s.Path("logs", "caddy.log")},
		{Label: launchd.LabelBerthd, Args: []string{berthd}, Env: env, Log: s.Path("logs", "berthd.log")},
	}
	if !cfg.NoTunnel {
		agents = append(agents, launchd.Agent{
			Label: launchd.LabelCloudflared,
			Args:  cloudflaredArgs(s, cfg),
			Env:   env,
			Log:   s.Path("logs", "cloudflared.log"),
		})
	}
	step("Installing LaunchAgents in %s", launchd.AgentsDir())
	for _, a := range agents {
		if err := launchd.Install(a); err != nil {
			return err
		}
		note("%s", a.Label)
	}
	return nil
}

func cloudflaredArgs(s *state.Store, cfg *state.Config) []string {
	bin := runtime.LookBin("cloudflared")
	if cfg.TunnelRemote {
		return []string{bin, "tunnel", "--metrics", cfg.TunnelMetrics, "--no-autoupdate", "run", "--token-file", tunnel.TokenPath(s.SecretsDir(), cfg.TunnelID)}
	}
	return []string{bin, "tunnel", "--config", s.Path("cloudflared.yml"), "--metrics", cfg.TunnelMetrics, "--no-autoupdate", "run", cfg.TunnelID}
}

func berthdPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	p := filepath.Join(filepath.Dir(exe), "berthd")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("berthd not found next to %s", exe)
	}
	return p, nil
}

func waitForDaemon(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := client().Ping(context.Background()); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return rpc.ErrDaemonDown
}
