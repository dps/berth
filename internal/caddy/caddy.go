// Package caddy generates Berth's complete Caddy JSON config from state and
// applies it through the admin API. The whole config is rebuilt and loaded
// on every change: /load is atomic and graceful in Caddy (listeners are
// reused, in-flight requests finish on the old config), so a deploy's
// upstream swap drops no connections, and a corrupted caddy.json is never
// fatal because it is always regenerated.
package caddy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// Route is one app's routing entry.
type Route struct {
	App         string
	PublicHosts []string // reached through the tunnel (127.0.0.1:80 server)
	LANHosts    []string // reached on the LAN (:443, internal CA)
	Upstreams   []string // the app's two slot ports; empty = no release (503)
	AuthUser    string
	AuthHash    string // bcrypt
	AccessLog   string
}

type Options struct {
	Admin       string // 127.0.0.1:2019
	HTTPListen  string // 127.0.0.1:80
	HTTPSListen string // :443, empty disables the LAN server
	LANHTTP     string // :80 redirect to HTTPS on the LAN, empty disables
	StorageDir  string // Caddy's data dir (internal CA lives here)
	Routes      []Route
}

type obj = map[string]any

// Build returns the full Caddy config.
func Build(o Options) obj {
	routes := append([]Route(nil), o.Routes...)
	sort.Slice(routes, func(i, j int) bool { return routes[i].App < routes[j].App })

	logs := obj{"default": obj{"exclude": []string{"http.log.access"}}}
	tunnelRoutes := []any{}
	lanRoutes := []any{}
	tunnelLoggers := obj{}
	lanLoggers := obj{}
	var lanSubjects []string

	for _, r := range routes {
		logger := "app-" + r.App
		if r.AccessLog != "" {
			logs[logger] = obj{
				"writer":  obj{"output": "file", "filename": r.AccessLog, "roll_size_mb": 50, "roll_keep": 5},
				"encoder": obj{"format": "json"},
				"include": []string{"http.log.access." + logger},
			}
		}
		h := handlers(r)
		// Every config change is a full graceful reload in Caddy, which closes
		// idle keep-alive connections; a client that reuses one at that
		// instant sees "connection closed". The tunnel server's only client is
		// cloudflared on loopback, so it gets one connection per request and
		// deploys drop nothing. (The LAN server keeps keep-alive; browsers
		// retry idempotent requests on this race.)
		th := append([]any{obj{"handler": "headers", "response": obj{"set": obj{"Connection": []string{"close"}}}}}, h...)
		if len(r.PublicHosts) > 0 {
			tunnelRoutes = append(tunnelRoutes, obj{"match": []any{obj{"host": r.PublicHosts}}, "handle": th, "terminal": true})
			for _, host := range r.PublicHosts {
				tunnelLoggers[host] = []string{logger}
			}
		}
		if len(r.LANHosts) > 0 {
			// The tunnel server also answers LAN hostnames over plain HTTP on
			// loopback, which makes local testing (curl -H Host:) easy.
			tunnelRoutes = append(tunnelRoutes, obj{"match": []any{obj{"host": r.LANHosts}}, "handle": th, "terminal": true})
			lanRoutes = append(lanRoutes, obj{"match": []any{obj{"host": r.LANHosts}}, "handle": h, "terminal": true})
			lanSubjects = append(lanSubjects, r.LANHosts...)
			for _, host := range r.LANHosts {
				tunnelLoggers[host] = []string{logger}
				lanLoggers[host] = []string{logger}
			}
		}
	}
	notFound := obj{"handle": []any{obj{
		"handler":     "static_response",
		"status_code": 404,
		"body":        "no app at this address (berth)\n",
		"headers":     obj{"Content-Type": []string{"text/plain; charset=utf-8"}},
	}}}
	tunnelRoutes = append(tunnelRoutes, notFound)
	lanRoutes = append(lanRoutes, notFound)

	servers := obj{
		"tunnel": obj{
			"listen":          []string{o.HTTPListen},
			"routes":          tunnelRoutes,
			"automatic_https": obj{"disable": true},
			"logs":            obj{"logger_names": tunnelLoggers, "skip_unmapped_hosts": true},
			// cloudflared connects from loopback and sets X-Forwarded-Proto:
			// https; trusting it lets apps see the visitor's real scheme and IP.
			"trusted_proxies":   obj{"source": "static", "ranges": []string{"127.0.0.1/32", "::1/128"}},
			"client_ip_headers": []string{"Cf-Connecting-Ip", "X-Forwarded-For"},
		},
	}
	httpApp := obj{"servers": servers}
	cfg := obj{
		"admin":   obj{"listen": o.Admin},
		"logging": obj{"logs": logs},
		"apps":    obj{"http": httpApp},
	}
	if o.StorageDir != "" {
		cfg["storage"] = obj{"module": "file_system", "root": o.StorageDir}
	}
	if o.HTTPSListen != "" {
		servers["lan"] = obj{
			"listen":                  []string{o.HTTPSListen},
			"routes":                  lanRoutes,
			"automatic_https":         obj{"disable_redirects": true},
			"tls_connection_policies": []any{obj{}},
			"logs":                    obj{"logger_names": lanLoggers, "skip_unmapped_hosts": true},
		}
		// Keep Caddy from trying to bind its own HTTP port for redirects or
		// ACME challenges: the tunnel server owns HTTP on loopback.
		httpApp["http_port"] = portOf(o.HTTPListen)
		httpApp["https_port"] = portOf(o.HTTPSListen)
		if o.LANHTTP != "" {
			loc := "https://{http.request.host}{http.request.uri}"
			if p := portOf(o.HTTPSListen); p != 443 {
				loc = fmt.Sprintf("https://{http.request.host}:%d{http.request.uri}", p)
			}
			servers["lan_redirect"] = obj{
				"listen":          []string{o.LANHTTP},
				"automatic_https": obj{"disable": true},
				"routes": []any{obj{"handle": []any{obj{
					"handler":     "static_response",
					"status_code": 308,
					"headers":     obj{"Location": []string{loc}},
				}}}},
			}
		}
		// Never let Caddy shell out to sudo to trust its CA; `berth init`
		// offers to add the root to the login keychain instead.
		cfg["apps"].(obj)["pki"] = obj{"certificate_authorities": obj{"local": obj{"install_trust": false}}}
		if len(lanSubjects) > 0 {
			sort.Strings(lanSubjects)
			cfg["apps"].(obj)["tls"] = obj{"automation": obj{"policies": []any{obj{
				"subjects": lanSubjects,
				"issuers":  []any{obj{"module": "internal"}},
			}}}}
		}
	}
	return cfg
}

func handlers(r Route) []any {
	var hs []any
	if r.AuthUser != "" {
		hs = append(hs, obj{
			"handler": "authentication",
			"providers": obj{"http_basic": obj{
				"accounts": []any{obj{"username": r.AuthUser, "password": base64.StdEncoding.EncodeToString([]byte(r.AuthHash))}},
				"hash":     obj{"algorithm": "bcrypt"},
				"realm":    r.App,
			}},
		})
	}
	if len(r.Upstreams) == 0 {
		return append(hs, obj{
			"handler":     "static_response",
			"status_code": 503,
			"body":        r.App + " is not running (berth status " + r.App + ")\n",
		})
	}
	hs = append(hs,
		obj{"handler": "encode", "encodings": obj{"zstd": obj{}, "gzip": obj{}}, "prefer": []string{"zstd", "gzip"}},
		obj{
			"handler":   "reverse_proxy",
			"upstreams": dials(r.Upstreams),
			// The first slot that accepts a connection gets the request. A
			// deploy starts the new release in the idle slot and stops the old
			// one; refused connections are retried on the other slot, so the
			// route never changes and Caddy never reloads during a deploy.
			"load_balancing": obj{
				"selection_policy": obj{"policy": "first"},
				"try_duration":     "5s",
				"try_interval":     "50ms",
			},
			"health_checks": obj{"passive": obj{"fail_duration": "1s", "max_fails": 1}},
			// No pooled connections to a release that may be stopping.
			"transport": obj{"protocol": "http", "keep_alive": obj{"enabled": false}},
		},
	)
	return hs
}

func dials(addrs []string) []any {
	out := make([]any, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, obj{"dial": a})
	}
	return out
}

func portOf(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			var p int
			fmt.Sscan(addr[i+1:], &p)
			return p
		}
	}
	return 0
}

// Client talks to Caddy's admin API.
type Client struct {
	Admin string
	HTTP  *http.Client
}

func NewClient(admin string) *Client {
	return &Client{Admin: admin, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Load replaces Caddy's running config.
func (c *Client) Load(ctx context.Context, cfg any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+c.Admin+"/load", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin API unreachable at %s: %w", c.Admin, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy rejected config: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return nil
}

// Ping reports whether the admin API answers.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+c.Admin+"/config/", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("caddy admin: %s", resp.Status)
	}
	return nil
}
