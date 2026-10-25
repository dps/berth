package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

// startAuth runs the sign-in server on cfg.AuthAddr (see design.md, Sign-in).
func (d *Daemon) startAuth(cfg *state.Config) (*http.Server, error) {
	os.MkdirAll(d.Store.SecretsDir(), 0o700)
	key, err := auth.LoadKey(filepath.Join(d.Store.SecretsDir(), "auth.key"))
	if err != nil {
		return nil, fmt.Errorf("sign-in key: %w", err)
	}
	d.auth = auth.NewServer(auth.OpenSessions(d.Store.Path("auth", "sessions.json")), key)
	d.configureAuth()
	ln, err := net.Listen("tcp", cfg.AuthAddr)
	if err != nil {
		return nil, fmt.Errorf("sign-in listener %s: %w", cfg.AuthAddr, err)
	}
	srv := &http.Server{Handler: d.auth, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go srv.Serve(ln)
	return srv, nil
}

// configureAuth loads the provider and groups from state into the server.
func (d *Daemon) configureAuth() {
	if d.auth == nil {
		return
	}
	ac, err := d.Store.AuthConfig()
	if err != nil {
		slog.Warn("auth config", "err", err)
		return
	}
	var p auth.Provider
	if ac.Provider == "google" && ac.ClientID != "" {
		if secret := d.secret("google.client_secret"); secret != "" {
			p = auth.NewGoogle(ac.ClientID, secret)
		}
	}
	d.auth.Configure(p, ac.Host, ac.Groups)
}

// signInHost is auth.<domain> once a provider is set up, else "".
func (d *Daemon) signInHost(cfg *state.Config) string {
	ac, err := d.Store.AuthConfig()
	if err != nil || ac.Provider == "" || cfg.Domain == "" || cfg.NoTunnel {
		return ""
	}
	return ac.Host
}

type AuthSetupParams struct {
	Provider     string `json:"provider"`
	Host         string `json:"host,omitempty"` // default auth.<domain>
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	CheckOnly    bool   `json:"check_only,omitempty"` // validate the host, change nothing
	Owner        string `json:"owner,omitempty"`      // email for the @owners group (dev versions)
}

// checkSignInHost makes sure host is <label>.<domain> (covered by the
// wildcard DNS record, tunnel rule and Cloudflare certificate), isn't an
// app, and has no DNS record of its own pointing somewhere else.
func (d *Daemon) checkSignInHost(ctx context.Context, cfg *state.Config, host string) error {
	label, rest, _ := strings.Cut(host, ".")
	if err := state.ValidateHostname(host); err != nil || rest != cfg.Domain {
		return fmt.Errorf("the sign-in host must be a name directly under %s, like auth.%s", cfg.Domain, cfg.Domain)
	}
	if label == "hooks" || d.Store.AppExists(label) {
		return fmt.Errorf("%s is already used by Berth or an app; pick another with --host", host)
	}
	_, cert := tunnel.CertForHost(d.zoneCertDir(), host)
	if cert == "" {
		return nil
	}
	oc, err := tunnel.ReadOriginCert(cert)
	if err != nil {
		return nil
	}
	recs, err := d.CloudflareAPI(oc.APIToken).Records(ctx, oc.ZoneID, host)
	if err != nil {
		slog.Warn("sign-in host DNS check", "host", host, "err", err)
		return nil
	}
	target := cfg.TunnelID + ".cfargotunnel.com"
	for _, r := range recs {
		if !(r.Type == "CNAME" && r.Content == target) {
			return fmt.Errorf("%s already has its own DNS record (%s %s), so it doesn't reach Berth. Pick another name with --host, e.g. --host signin.%s", host, r.Type, r.Content, cfg.Domain)
		}
	}
	return nil
}

func (d *Daemon) hAuthSetup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AuthSetupParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	cfg := d.cfg()
	if p.Provider != "google" {
		return nil, fmt.Errorf("unknown sign-in provider %q (google)", p.Provider)
	}
	if cfg.Domain == "" || cfg.NoTunnel {
		return nil, fmt.Errorf("sign-in needs the Cloudflare tunnel and a domain: Google only redirects to public hostnames")
	}
	ac, err := d.Store.AuthConfig()
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(strings.TrimSpace(p.Host))
	if host == "" {
		host = ac.Host
	}
	if host == "" {
		host = "auth." + cfg.Domain
	}
	if err := d.checkSignInHost(ctx, cfg, host); err != nil {
		return nil, err
	}
	if p.CheckOnly {
		return map[string]string{"host": host}, nil
	}
	p.ClientID, p.ClientSecret = strings.TrimSpace(p.ClientID), strings.TrimSpace(p.ClientSecret)
	if !strings.HasSuffix(p.ClientID, ".apps.googleusercontent.com") || strings.ContainsAny(p.ClientID, " \t\r\n") {
		return nil, fmt.Errorf("that doesn't look like a Google OAuth client ID (…apps.googleusercontent.com)")
	}
	if p.ClientSecret == "" || strings.ContainsAny(p.ClientSecret, " \t\r\n") {
		return nil, fmt.Errorf("missing client secret")
	}
	if err := os.MkdirAll(d.Store.SecretsDir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(d.Store.SecretsDir(), "google.client_secret"), []byte(p.ClientSecret+"\n"), 0o600); err != nil {
		return nil, err
	}
	ac.Provider, ac.ClientID, ac.Host = "google", p.ClientID, host
	if owner := strings.ToLower(strings.TrimSpace(p.Owner)); owner != "" {
		if err := state.ValidateAllowEntry(owner); err != nil || strings.HasPrefix(owner, "@") {
			return nil, fmt.Errorf("--owner takes an email address")
		}
		if !slices.Contains(ac.Groups[devOwnersGroup], owner) {
			ac.Groups[devOwnersGroup] = append(ac.Groups[devOwnersGroup], owner)
		}
		fmt.Fprintf(out, "-----> @%s: %s (dev versions are open to this group)\n", devOwnersGroup, strings.Join(ac.Groups[devOwnersGroup], " "))
	}
	if err := d.Store.SaveAuthConfig(ac); err != nil {
		return nil, err
	}
	d.configureAuth()
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	d.event(state.Event{Kind: "auth", Message: "Google sign-in set up at " + ac.Host})
	fmt.Fprintf(out, "-----> Google sign-in is set up at https://%s\n", ac.Host)
	return map[string]string{"provider": "google", "host": ac.Host}, nil
}

type AuthSession struct {
	Email   string    `json:"email"`
	Name    string    `json:"name,omitempty"`
	Host    string    `json:"host"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type AuthStatus struct {
	Provider string              `json:"provider,omitempty"`
	ClientID string              `json:"client_id,omitempty"`
	Host     string              `json:"host,omitempty"`
	Groups   map[string][]string `json:"groups"`
	Apps     map[string]string   `json:"apps"` // app -> policy
	Sessions []AuthSession       `json:"sessions"`
}

func (d *Daemon) hAuthStatus(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	ac, err := d.Store.AuthConfig()
	if err != nil {
		return nil, err
	}
	st := AuthStatus{Provider: ac.Provider, ClientID: ac.ClientID, Host: ac.Host, Groups: ac.Groups, Apps: map[string]string{}, Sessions: []AuthSession{}}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		if a.Access != nil {
			st.Apps[a.Name] = a.Access.String()
		}
	}
	if d.auth != nil {
		for _, s := range d.auth.Sessions.List() {
			st.Sessions = append(st.Sessions, AuthSession{Email: s.User.Email, Name: s.User.Name, Host: s.Host, Created: s.Created, Expires: s.Expires})
		}
	}
	return st, nil
}

type AuthGroupParams struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
	Delete  bool     `json:"delete,omitempty"`
}

func (d *Daemon) hAuthGroup(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AuthGroupParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := state.ValidateGroupName(p.Name); err != nil {
		return nil, err
	}
	members := normalizeAllow(p.Members)
	for _, m := range members {
		if err := state.ValidateAllowEntry(m); err != nil {
			return nil, err
		}
		if strings.HasPrefix(m, "@") && !strings.Contains(m, ".") {
			return nil, fmt.Errorf("groups hold emails and @domains, not other groups (%s)", m)
		}
	}
	if !p.Delete && len(members) == 0 {
		return nil, fmt.Errorf("usage: berth auth:group <name> <email|@domain>... (or --delete)")
	}
	ac, err := d.Store.AuthConfig()
	if err != nil {
		return nil, err
	}
	if p.Delete {
		delete(ac.Groups, p.Name)
		fmt.Fprintf(out, "-----> Deleted group @%s\n", p.Name)
	} else {
		ac.Groups[p.Name] = members
		fmt.Fprintf(out, "-----> @%s: %s\n", p.Name, strings.Join(members, " "))
	}
	if err := d.Store.SaveAuthConfig(ac); err != nil {
		return nil, err
	}
	d.configureAuth()
	return map[string]any{"group": p.Name, "members": ac.Groups[p.Name]}, nil
}

type AuthRevokeParams struct {
	Email string `json:"email"`
}

func (d *Daemon) hAuthRevoke(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AuthRevokeParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	email := strings.ToLower(strings.TrimSpace(p.Email))
	if email == "" || d.auth == nil {
		return nil, fmt.Errorf("usage: berth auth:revoke <email>")
	}
	n := d.auth.Sessions.EndUser("", email)
	d.event(state.Event{Kind: "auth", Message: fmt.Sprintf("signed %s out (%d sessions)", email, n)})
	fmt.Fprintf(out, "-----> Ended %d session(s) for %s\n", n, email)
	return map[string]any{"email": email, "ended": n}, nil
}

type AccessParams struct {
	App    string   `json:"app"`
	Path   string   `json:"path,omitempty"`
	Level  string   `json:"level,omitempty"`
	Allow  []string `json:"allow,omitempty"`
	Remove bool     `json:"remove,omitempty"` // drop the path rule
	Off    bool     `json:"off,omitempty"`    // remove the whole policy
}

func normalizeAllow(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range in {
		for _, x := range strings.FieldsFunc(w, func(r rune) bool { return r == ',' || r == ' ' }) {
			x = strings.ToLower(x)
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	return out
}

func (d *Daemon) hAccessSet(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AccessParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	p.Allow = normalizeAllow(p.Allow)
	if p.Path != "" {
		if err := state.ValidateAccessPath(p.Path); err != nil {
			return nil, err
		}
	}
	if !p.Off && !p.Remove {
		if err := state.ValidateLevel(p.Level, p.Allow); err != nil {
			return nil, err
		}
	}
	var result *state.Access
	err := d.updateApp(p.App, func(a *state.App) {
		switch {
		case p.Off:
			a.Access = nil
		case p.Path != "":
			if a.Access == nil {
				a.Access = &state.Access{Level: state.LevelPublic}
			}
			if p.Remove {
				a.Access.SetPath(p.Path, "", nil)
			} else {
				a.Access.SetPath(p.Path, p.Level, p.Allow)
			}
		default:
			if a.Access == nil {
				a.Access = &state.Access{}
			}
			a.Access.Level, a.Access.Allow = p.Level, p.Allow
		}
		result = a.Access
	})
	if err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	if result == nil {
		fmt.Fprintf(out, "-----> Sign-in off for %s\n", p.App)
		return map[string]any{"app": p.App, "access": nil}, nil
	}
	fmt.Fprintf(out, "-----> %s: %s\n", p.App, result.String())
	ac, _ := d.Store.AuthConfig()
	var unknown []string
	for _, w := range append(append([]string{}, result.Allow...), pathAllows(result)...) {
		if g, ok := strings.CutPrefix(w, "@"); ok && !strings.Contains(g, ".") && ac.Groups[g] == nil {
			unknown = append(unknown, w)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fmt.Fprintf(out, "       note: no group %s yet (berth auth:group <name> <emails...>)\n", strings.Join(unknown, ", "))
	}
	if ac.Provider == "" {
		fmt.Fprintf(out, "       Sign-in isn't set up yet, so nobody can sign in: berth auth:setup google\n")
	}
	return map[string]any{"app": p.App, "access": result}, nil
}

func pathAllows(a *state.Access) []string {
	var out []string
	for _, p := range a.Paths {
		out = append(out, p.Allow...)
	}
	return out
}
