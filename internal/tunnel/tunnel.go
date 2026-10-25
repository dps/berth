// Package tunnel manages cloudflared: the ingress list, the locally managed
// config file, the remotely managed configuration API used for custom
// domains (no restart), DNS records and the optional webhook IP filter.
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rule is one cloudflared ingress rule.
type Rule struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

// Ingress builds the ingress list: the webhook host, custom domains, the
// app wildcard, then a 404 catch-all. App names never appear here.
func Ingress(domain, webhookAddr, caddyHTTP string, customDomains []string) []Rule {
	caddy := "http://" + caddyHTTP
	rules := []Rule{{Hostname: "hooks." + domain, Service: "http://" + webhookAddr}}
	cd := append([]string(nil), customDomains...)
	sort.Strings(cd)
	for _, d := range cd {
		rules = append(rules, Rule{Hostname: d, Service: caddy})
	}
	rules = append(rules, Rule{Hostname: "*." + domain, Service: caddy}, Rule{Service: "http_status:404"})
	return rules
}

// LocalConfig renders ~/.berth/cloudflared.yml.
func LocalConfig(tunnelID, credentialsFile string, rules []Rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by berthd; regenerated from state on every start.\n")
	fmt.Fprintf(&b, "tunnel: %s\ncredentials-file: %s\ningress:\n", tunnelID, credentialsFile)
	for _, r := range rules {
		if r.Hostname != "" {
			fmt.Fprintf(&b, "  - hostname: %q\n    service: %s\n", r.Hostname, r.Service)
		} else {
			fmt.Fprintf(&b, "  - service: %s\n", r.Service)
		}
	}
	return b.String()
}

// Ready asks cloudflared's metrics server whether the tunnel has at least
// one live connection to the edge.
func Ready(ctx context.Context, metricsAddr string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+metricsAddr+"/ready", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// ---- cloudflared CLI ----

// CLI runs cloudflared tunnel commands with an explicit origin cert, so
// Berth never depends on ~/.cloudflared.
type CLI struct {
	Bin    string
	Origin string // origin cert from `cloudflared tunnel login`
}

func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	if c.Origin != "" && len(args) > 0 && args[0] == "tunnel" {
		args = append([]string{"tunnel", "--origincert", c.Origin}, args[1:]...)
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("cloudflared %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// Login runs `cloudflared tunnel login`, which opens the browser for the
// user to pick a zone, and saves the resulting origin cert at dest. It runs
// with a temporary HOME so an existing ~/.cloudflared/cert.pem is neither
// in the way nor overwritten.
func (c *CLI) Login(ctx context.Context, dest string, stdout io.Writer) (*OriginCert, error) {
	home, err := os.MkdirTemp("", "berth-cf-login-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	cmd := exec.CommandContext(ctx, c.Bin, "tunnel", "login")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout, cmd.Stderr = stdout, stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cloudflared tunnel login: %w", err)
	}
	src := filepath.Join(home, ".cloudflared", "cert.pem")
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("cloudflared tunnel login did not write a cert: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dest, b, 0o600); err != nil {
		return nil, err
	}
	return ParseOriginCert(b)
}

// OriginCert is what `cloudflared tunnel login` hands back: an API token
// scoped to the zone the user picked in the browser (tunnel management and
// DNS for that zone), plus the zone and account ids.
type OriginCert struct {
	ZoneID    string `json:"zoneID"`
	AccountID string `json:"accountID"`
	APIToken  string `json:"apiToken"`
}

func ReadOriginCert(path string) (*OriginCert, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseOriginCert(b)
}

// ParseOriginCert reads the token block of a cloudflared cert.pem.
func ParseOriginCert(b []byte) (*OriginCert, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("origin cert has no tunnel token block")
		}
		if !strings.Contains(blk.Type, "TOKEN") {
			continue
		}
		c := &OriginCert{}
		if err := json.Unmarshal(blk.Bytes, c); err != nil {
			return nil, fmt.Errorf("origin cert token: %w", err)
		}
		if c.APIToken == "" || c.ZoneID == "" {
			return nil, fmt.Errorf("origin cert token is incomplete")
		}
		return c, nil
	}
}

type tunnelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindTunnel returns the id of a named tunnel, or "".
func (c *CLI) FindTunnel(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, "tunnel", "list", "--output", "json", "--name", name)
	if err != nil {
		return "", err
	}
	var ts []tunnelInfo
	if err := json.Unmarshal([]byte(out), &ts); err != nil {
		return "", nil
	}
	for _, t := range ts {
		if t.Name == name {
			return t.ID, nil
		}
	}
	return "", nil
}

// Create makes a tunnel and writes its credentials file.
func (c *CLI) Create(ctx context.Context, name, credFile string) (string, error) {
	if _, err := c.run(ctx, "tunnel", "create", "--credentials-file", credFile, name); err != nil {
		return "", err
	}
	return c.FindTunnel(ctx, name)
}

// FetchCredentials writes an existing tunnel's credentials file.
func (c *CLI) FetchCredentials(ctx context.Context, name, credFile string) error {
	_, err := c.run(ctx, "tunnel", "token", "--cred-file", credFile, name)
	return err
}

// Token returns the tunnel's run token; `cloudflared tunnel run --token`
// makes cloudflared follow the remotely managed configuration.
func (c *CLI) Token(ctx context.Context, tunnel string) (string, error) {
	out, err := c.run(ctx, "tunnel", "token", tunnel)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("cloudflared tunnel token: no output")
	}
	return fields[len(fields)-1], nil
}

// TokenPath is where Berth keeps a tunnel's run token.
func TokenPath(secretsDir, tunnelID string) string {
	return filepath.Join(secretsDir, "cloudflared", tunnelID+".token")
}

// RunningConfigVersion asks cloudflared's metrics server which ingress
// version it is using: -1 means the local config file, >= 1 a remote one.
func RunningConfigVersion(ctx context.Context, metricsAddr string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+metricsAddr+"/config", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var c struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return 0, err
	}
	return c.Version, nil
}

// RouteDNS points hostname (which must be in the origin cert's zone) at
// the tunnel with a proxied CNAME.
func (c *CLI) RouteDNS(ctx context.Context, tunnel, hostname string) error {
	_, err := c.run(ctx, "tunnel", "route", "dns", "--overwrite-dns", tunnel, hostname)
	return err
}

// ---- Cloudflare API (login-cert token or a scoped token) ----

type API struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

func NewAPI(token string) *API {
	return &API{Token: token, BaseURL: "https://api.cloudflare.com/client/v4", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

type cfResponse struct {
	Success bool                       `json:"success"`
	Errors  []struct{ Message string } `json:"errors"`
	Result  json.RawMessage            `json:"result"`
}

func (a *API) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r cfResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return fmt.Errorf("cloudflare %s %s: %s", method, path, resp.Status)
	}
	if !r.Success {
		var msgs []string
		for _, e := range r.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("cloudflare %s %s: %s", method, path, strings.Join(msgs, "; "))
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (a *API) Verify(ctx context.Context) error {
	return a.do(ctx, "GET", "/user/tokens/verify", nil, nil)
}

type Zone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

// ZoneByID looks up a zone's name.
func (a *API) ZoneByID(ctx context.Context, id string) (*Zone, error) {
	z := &Zone{}
	return z, a.do(ctx, "GET", "/zones/"+id, nil, z)
}

// ZoneFor finds the zone that hostname belongs to (longest suffix match).
func (a *API) ZoneFor(ctx context.Context, hostname string) (*Zone, error) {
	labels := strings.Split(hostname, ".")
	for i := 0; i < len(labels)-1; i++ {
		name := strings.Join(labels[i:], ".")
		var zones []Zone
		if err := a.do(ctx, "GET", "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return nil, err
		}
		if len(zones) > 0 {
			return &zones[0], nil
		}
	}
	return nil, fmt.Errorf("no Cloudflare zone for %s on this account", hostname)
}

// PutTunnelConfig switches the tunnel to remotely managed ingress;
// connected cloudflared instances apply it live without a restart.
func (a *API) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []Rule) error {
	body := map[string]any{"config": map[string]any{"ingress": rules}}
	return a.do(ctx, "PUT", "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations", body, nil)
}

type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

type DNSRecord = dnsRecord

// Records lists the DNS records for one name.
func (a *API) Records(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	var recs []dnsRecord
	err := a.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?name="+url.QueryEscape(name), nil, &recs)
	return recs, err
}

// UpsertCNAME points hostname at the tunnel (proxied).
func (a *API) UpsertCNAME(ctx context.Context, zoneID, hostname, target string) error {
	var recs []dnsRecord
	if err := a.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?name="+url.QueryEscape(hostname), nil, &recs); err != nil {
		return err
	}
	body := map[string]any{"type": "CNAME", "name": hostname, "content": target, "proxied": true, "comment": "berth"}
	for _, r := range recs {
		if r.Type == "CNAME" {
			return a.do(ctx, "PUT", "/zones/"+zoneID+"/dns_records/"+r.ID, body, nil)
		}
		return fmt.Errorf("%s already has a %s record; remove it first", hostname, r.Type)
	}
	return a.do(ctx, "POST", "/zones/"+zoneID+"/dns_records", body, nil)
}

func (a *API) DeleteCNAME(ctx context.Context, zoneID, hostname, target string) error {
	var recs []dnsRecord
	if err := a.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?type=CNAME&name="+url.QueryEscape(hostname), nil, &recs); err != nil {
		return err
	}
	for _, r := range recs {
		if r.Content == target {
			if err := a.do(ctx, "DELETE", "/zones/"+zoneID+"/dns_records/"+r.ID, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

const filterRuleRef = "berth_webhook_ip_filter"

type rulesetRule struct {
	ID          string `json:"id,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Expression  string `json:"expression"`
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// FilterExpression blocks requests to the webhook host unless they come
// from GitHub's hook ranges.
func FilterExpression(hooksHost string, ranges []string) string {
	return fmt.Sprintf(`(http.host eq "%s" and not ip.src in {%s})`, hooksHost, strings.Join(ranges, " "))
}

// SetWebhookFilter installs or updates Berth's WAF custom rule, preserving
// any rules the user already has in the zone's custom-rules phase.
func (a *API) SetWebhookFilter(ctx context.Context, zoneID, hooksHost string, ranges []string) error {
	path := "/zones/" + zoneID + "/rulesets/phases/http_request_firewall_custom/entrypoint"
	var rs struct {
		Rules []rulesetRule `json:"rules"`
	}
	if err := a.do(ctx, "GET", path, nil, &rs); err != nil && !strings.Contains(err.Error(), "not found") {
		return err
	}
	ours := rulesetRule{Ref: filterRuleRef, Expression: FilterExpression(hooksHost, ranges), Action: "block", Description: "berth: only GitHub may call the webhook endpoint", Enabled: true}
	var rules []rulesetRule
	replaced := false
	for _, r := range rs.Rules {
		if r.Ref == filterRuleRef {
			ours.ID = r.ID
			rules = append(rules, ours)
			replaced = true
			continue
		}
		rules = append(rules, r)
	}
	if !replaced {
		rules = append(rules, ours)
	}
	return a.do(ctx, "PUT", path, map[string]any{"rules": rules}, nil)
}

// ---- zone certs ----

// ZoneCertPath is where Berth keeps the login cert for one Cloudflare zone.
func ZoneCertPath(dir, zone string) string { return filepath.Join(dir, zone+".pem") }

// CertForHost finds the login cert whose zone contains hostname (longest
// match), or "" if no connected zone covers it.
func CertForHost(dir, hostname string) (zone, path string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		z, ok := strings.CutSuffix(e.Name(), ".pem")
		if !ok {
			continue
		}
		if (hostname == z || strings.HasSuffix(hostname, "."+z)) && len(z) > len(zone) {
			zone, path = z, filepath.Join(dir, e.Name())
		}
	}
	return zone, path
}
