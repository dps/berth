// Package analytics turns Caddy's per-app access logs into the numbers the
// admin page shows: requests over time by status class, latency
// percentiles, unique visitors, top paths, referrers, countries and
// browsers, and the most recent requests. Everything is computed from the
// logs Berth already writes, in memory, for the last 7 days; nothing is
// added to the request path and no visitor data leaves the Mac.
package analytics

import (
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"time"
)

// Request is one access-log entry, reduced to what the stats need.
type Request struct {
	Time     time.Time `json:"t"`
	Method   string    `json:"method"`
	Host     string    `json:"host"`
	Path     string    `json:"path"` // query string dropped
	Status   int       `json:"status"`
	Ms       float64   `json:"ms"`
	Bytes    int64     `json:"bytes"`
	IP       string    `json:"ip"`
	Country  string    `json:"country,omitempty"`  // from Cloudflare's CF-IPCountry
	Referrer string    `json:"referrer,omitempty"` // another site's host, if any
	Browser  string    `json:"browser"`
	Mobile   bool      `json:"mobile,omitempty"`
	Bot      bool      `json:"bot,omitempty"`
	visitor  uint64
}

type caddyEntry struct {
	TS       float64 `json:"ts"`
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
	Status   int     `json:"status"`
	Request  struct {
		ClientIP string              `json:"client_ip"`
		RemoteIP string              `json:"remote_ip"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
}

// Parse reads one line of a Caddy JSON access log.
func Parse(line []byte) (Request, bool) {
	var e caddyEntry
	if json.Unmarshal(line, &e) != nil || e.TS == 0 || e.Request.Method == "" {
		return Request{}, false
	}
	sec, frac := math.Modf(e.TS)
	r := Request{
		Time:   time.Unix(int64(sec), int64(frac*1e9)),
		Method: e.Request.Method,
		Host:   e.Request.Host,
		Status: e.Status,
		Ms:     e.Duration * 1000,
		Bytes:  e.Size,
		IP:     e.Request.ClientIP,
	}
	if r.IP == "" {
		r.IP = e.Request.RemoteIP
	}
	path, _, _ := strings.Cut(e.Request.URI, "?")
	if len(path) > 200 {
		path = path[:200]
	}
	r.Path = path
	h := func(k string) string {
		if v := e.Request.Headers[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if c := h("Cf-Ipcountry"); len(c) == 2 && c != "XX" && c != "T1" {
		r.Country = c
	}
	if ref := h("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
			r.Referrer = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		}
	}
	ua := h("User-Agent")
	r.Browser, r.Mobile, r.Bot = classify(ua)
	r.visitor = fnv(r.IP + "\x00" + ua)
	return r, true
}

var botWords = []string{"bot", "crawl", "spider", "slurp", "preview", "monitor", "uptime", "headless", "scan", "facebookexternalhit", "python", "go-http-client", "curl/", "wget/", "httpie", "axios", "node-fetch", "undici", "okhttp", "java/", "libwww"}

// classify names the browser family from a User-Agent, and spots mobiles
// and non-browsers (crawlers, scripts, monitors).
func classify(ua string) (browser string, mobile, bot bool) {
	l := strings.ToLower(ua)
	if l == "" {
		return "Unknown", false, true
	}
	for _, w := range botWords {
		if strings.Contains(l, w) {
			switch {
			case strings.HasPrefix(l, "curl/"):
				return "curl", false, true
			case strings.Contains(l, "bot") || strings.Contains(l, "crawl") || strings.Contains(l, "spider"):
				return "Crawler", false, true
			}
			return "Script", false, true
		}
	}
	if !strings.HasPrefix(l, "mozilla/") {
		return "Script", false, true // every browser's User-Agent starts with Mozilla/
	}
	mobile = strings.Contains(l, "mobile") || strings.Contains(l, "android") || strings.Contains(l, "iphone") || strings.Contains(l, "ipad")
	switch {
	case strings.Contains(l, "edg/"):
		return "Edge", mobile, false
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		return "Opera", mobile, false
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios"):
		return "Firefox", mobile, false
	case strings.Contains(l, "samsungbrowser"):
		return "Samsung Internet", mobile, false
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios"):
		return "Chrome", mobile, false
	case strings.Contains(l, "safari/"):
		return "Safari", mobile, false
	}
	return "Other", mobile, false
}

// NormalizePath folds IDs out of paths so /api/chirps/12/like and
// /api/chirps/13/like count as one: segments that are numbers, UUIDs or
// long hex strings become ":id".
func NormalizePath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if isID(s) {
			segs[i] = ":id"
		}
	}
	return strings.Join(segs, "/")
}

func isID(s string) bool {
	if s == "" {
		return false
	}
	digits, hex := true, len(s) >= 16
	for _, c := range s {
		if c < '0' || c > '9' {
			digits = false
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-') {
			hex = false
		}
	}
	return digits || hex
}

func fnv(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
