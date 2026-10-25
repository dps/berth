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
	Service     string   // SRV service name (the app); empty = no running release (503)
	AuthUser    string
	AuthHash    string // bcrypt
	AccessLog   string
	// SignIn routes /_berth/* to berthd's sign-in server and checks every
	// request with it (forward_auth); Private also marks responses
	// Cache-Control private so Cloudflare never caches them for others.
	SignIn  bool
	Private bool
	// Placeholder, if set, is all the route serves (a dev address before its
	// first push). DownBody replaces the "not running" text; NoIndex asks
	// search engines to stay away.
	Placeholder string
	DownBody    string
	NoIndex     bool
}

type Options struct {
	Admin       string // 127.0.0.1:2019
	HTTPListen  string // 127.0.0.1:80
	HTTPSListen string // :443, empty disables the LAN server
	DNSAddr     string // berthd's SRV responder, e.g. 127.0.0.1:7753
	SRVDomain   string // berth.internal
	AuthAddr    string // berthd's sign-in server, e.g. 127.0.0.1:7781
	AuthHost    string // auth.<domain>; empty when sign-in isn't set up
	// AIListen, when set, is where apps reach berthd's AI gateway (AIAddr):
	// a port on every interface, answering only AISources (the container
	// network and loopback). Caddy, not berthd, listens there because
	// macOS's firewall approves apps, not ports, and Caddy already serves
	// the LAN.
	AIListen   string
	AIAddr     string
	AISources  []string
	IDHeaders  []string // identity headers: always stripped from clients, copied from forward_auth
	LANHTTP    string   // :80 redirect to HTTPS on the LAN, empty disables
	StorageDir string   // Caddy's data dir (internal CA lives here)
	Routes     []Route
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
		h := handlers(r, o)
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
	if o.AuthHost != "" && o.AuthAddr != "" {
		tunnelRoutes = append([]any{obj{
			"match": []any{obj{"host": []string{o.AuthHost}}},
			"handle": []any{
				obj{"handler": "headers", "response": obj{"set": obj{"Connection": []string{"close"}}}},
				obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": o.AuthAddr}}},
			},
			"terminal": true,
		}}, tunnelRoutes...)
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
	if o.AIListen != "" && o.AIAddr != "" {
		servers["ai"] = obj{
			"listen":          []string{o.AIListen},
			"automatic_https": obj{"disable": true},
			"routes": []any{
				obj{
					"match": []any{obj{"remote_ip": obj{"ranges": o.AISources}}},
					"handle": []any{obj{
						"handler":        "reverse_proxy",
						"upstreams":      []any{obj{"dial": o.AIAddr}},
						"flush_interval": -1,
					}},
					"terminal": true,
				},
				obj{"handle": []any{obj{
					"handler":     "static_response",
					"status_code": 403,
					"body":        "the Berth AI gateway only answers Berth apps\n",
				}}},
			},
		}
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

func handlers(r Route, o Options) []any {
	var hs []any
	if len(o.IDHeaders) > 0 {
		// Identity headers only ever come from berthd, never the client.
		hs = append(hs, obj{"handler": "headers", "request": obj{"delete": o.IDHeaders}})
	}
	if r.NoIndex {
		hs = append(hs, obj{"handler": "headers", "response": obj{"set": obj{"X-Robots-Tag": []string{"noindex, nofollow"}}}})
	}
	if r.Placeholder != "" {
		return append(hs, obj{
			"handler":     "static_response",
			"status_code": 404,
			"body":        r.Placeholder,
			"headers":     obj{"Content-Type": []string{"text/plain; charset=utf-8"}},
		})
	}
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
	if r.SignIn && o.AuthAddr != "" {
		rest := appHandlers(r, o)
		if r.Private {
			rest = append([]any{obj{"handler": "headers", "response": obj{"deferred": true, "set": obj{"Cache-Control": []string{"private, no-cache"}}}}}, rest...)
		}
		rest = append([]any{forwardAuth(o)}, rest...)
		return append(hs, obj{"handler": "subroute", "routes": []any{
			obj{
				"match":    []any{obj{"path": []string{"/_berth/*"}}},
				"handle":   []any{obj{"handler": "reverse_proxy", "upstreams": []any{obj{"dial": o.AuthAddr}}}},
				"terminal": true,
			},
			obj{"handle": rest},
		}})
	}
	return append(hs, appHandlers(r, o)...)
}

// forwardAuth asks berthd about the request (what Caddy's forward_auth
// directive expands to): a 2xx lets it through with the identity headers
// copied onto it; anything else (redirect to sign in, 401, 403) goes back
// to the client as is.
func forwardAuth(o Options) obj {
	routes := []any{}
	for _, h := range o.IDHeaders {
		ph := "{http.reverse_proxy.header." + h + "}"
		routes = append(routes,
			obj{"handle": []any{obj{"handler": "headers", "request": obj{"delete": []string{h}}}}},
			obj{
				"match":  []any{obj{"not": []any{obj{"vars": obj{ph: []string{""}}}}}},
				"handle": []any{obj{"handler": "headers", "request": obj{"set": obj{h: []string{ph}}}}},
			})
	}
	return obj{
		"handler":   "reverse_proxy",
		"upstreams": []any{obj{"dial": o.AuthAddr}},
		"rewrite":   obj{"method": "GET", "uri": "/_berth/check"},
		"headers": obj{"request": obj{"set": obj{
			"X-Forwarded-Method": []string{"{http.request.method}"},
			"X-Forwarded-Uri":    []string{"{http.request.uri}"},
		}}},
		"handle_response": []any{obj{"match": obj{"status_code": []int{2}}, "routes": routes}},
	}
}

func appHandlers(r Route, o Options) []any {
	var hs []any
	if r.Service == "" {
		body := r.DownBody
		if body == "" {
			body = r.App + " is not running (berth status " + r.App + ")\n"
		}
		return append(hs, obj{
			"handler":     "static_response",
			"status_code": 503,
			"body":        body,
			"headers":     obj{"Retry-After": []string{"30"}},
		})
	}
	hs = append(hs,
		obj{"handler": "encode", "encodings": obj{"zstd": obj{}, "gzip": obj{}}, "prefer": []string{"zstd", "gzip"}},
		obj{
			"handler": "reverse_proxy",
			// The upstream is whatever berthd's SRV record for the app says:
			// the current release's port, re-resolved every second. A deploy
			// changes the record, never this config, so Caddy never reloads
			// and traffic moves within a second; the old release drains its
			// in-flight requests. If berthd is briefly away, the last answer
			// stays in use (grace_period).
			"dynamic_upstreams": obj{
				"source":       "srv",
				"service":      r.Service,
				"proto":        "tcp",
				"name":         o.SRVDomain,
				"refresh":      "1s",
				"grace_period": "1h",
				"dial_timeout": "2s",
				"resolver":     obj{"addresses": []string{"udp/" + o.DNSAddr}},
			},
			// Retry refused connections (a release restarting in place).
			"load_balancing": obj{"try_duration": "5s", "try_interval": "100ms"},
			// No pooled connections to a release that may be stopping.
			"transport": obj{"protocol": "http", "keep_alive": obj{"enabled": false}},
		},
	)
	return hs
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
