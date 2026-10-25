package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/state"
)

// Identity headers passed to apps. Caddy deletes any a client sends.
var Headers = []string{"X-Berth-User-Id", "X-Berth-Email", "X-Berth-Name", "X-Berth-Picture", "X-Berth-Identity"}

const (
	cookieSession = "__Host-berth_session"
	cookieLogin   = "__Host-berth_login" // app host: nonce for a sign-in in progress
	cookieOAuth   = "__Host-berth_oauth" // auth host: OAuth state
	loginTTL      = 10 * time.Minute
	codeTTL       = 60 * time.Second
	maxPending    = 10_000
)

// Site is an app hostname with an access policy.
type Site struct {
	App     string
	BaseURL string // https://host[:port], where /_berth/callback lives
	Access  *state.Access
	// Tokens (dev versions only) maps HashToken(token) to the pretend
	// user's email, or "" for no identity.
	Tokens map[string]string
}

// TokenPrefix marks dev tokens: Authorization: Bearer berth_dev_….
const TokenPrefix = "berth_dev_"

// NewToken returns a fresh dev token.
func NewToken() string { return TokenPrefix + token() }

// HashToken is how dev tokens are stored and looked up.
func HashToken(t string) string { return hashOf(t) }

type login struct {
	host, ret string
	exp       time.Time
}

type oauthState struct {
	host, n, verifier, nonce string
	exp                      time.Time
}

type handoff struct {
	host, n string
	user    Identity
	exp     time.Time
}

// Server answers forward_auth checks and runs the sign-in flow.
type Server struct {
	Sessions *Sessions
	Key      ed25519.PrivateKey

	mu       sync.Mutex
	provider Provider
	authHost string
	sites    map[string]Site
	groups   map[string][]string
	logins   map[string]*login // by sha256(nonce)
	oauth    map[string]*oauthState
	codes    map[string]*handoff
}

func NewServer(sessions *Sessions, key ed25519.PrivateKey) *Server {
	return &Server{
		Sessions: sessions, Key: key,
		sites: map[string]Site{}, groups: map[string][]string{},
		logins: map[string]*login{}, oauth: map[string]*oauthState{}, codes: map[string]*handoff{},
	}
}

// Configure sets the provider (nil when sign-in isn't set up), the auth
// host and the groups.
func (s *Server) Configure(p Provider, authHost string, groups map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider, s.authHost, s.groups = p, authHost, groups
}

// SetSites replaces the hostname → app map.
func (s *Server) SetSites(sites map[string]Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sites = sites
}

func (s *Server) snapshot() (Provider, string, map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider, s.authHost, s.groups
}

func (s *Server) site(host string) (Site, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sites[host]
	return st, ok
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(h)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	_, authHost, _ := s.snapshot()
	if authHost != "" && host == authHost {
		switch r.URL.Path {
		case "/":
			s.authHome(w, r, host)
		case "/start":
			s.start(w, r, host)
		case "/callback":
			s.callback(w, r, host)
		case "/logout":
			s.logout(w, r, host, "/")
		default:
			http.NotFound(w, r)
		}
		return
	}
	site, ok := s.site(host)
	if !ok {
		http.Error(w, "no app at this address (berth)", http.StatusNotFound)
		return
	}
	switch r.URL.Path {
	case "/_berth/check":
		s.check(w, r, host, site)
	case "/_berth/login":
		s.appLogin(w, r, host)
	case "/_berth/callback":
		s.appCallback(w, r, host)
	case "/_berth/logout":
		s.logout(w, r, host, safeReturn(r.URL.Query().Get("return")))
	case "/_berth/me":
		s.me(w, r, host)
	case "/_berth/jwks.json":
		w.Header().Set("Content-Type", "application/json")
		w.Write(JWKS(s.Key))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) session(r *http.Request, host string) *Session {
	c, err := r.Cookie(cookieSession)
	if err != nil {
		return nil
	}
	return s.Sessions.Get(host, c.Value)
}

// ---- forward_auth ----

func (s *Server) check(w http.ResponseWriter, r *http.Request, host string, site Site) {
	uri := r.Header.Get("X-Forwarded-Uri")
	if uri == "" {
		uri = "/"
	}
	method := r.Header.Get("X-Forwarded-Method")
	if method == "" {
		method = r.Method
	}
	p, ok := cleanPath(uri)
	if !ok {
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}
	_, authHost, groups := s.snapshot()
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "+TokenPrefix); ok && site.Tokens != nil {
		as, valid := site.Tokens[HashToken(TokenPrefix+tok)]
		if !valid {
			jsonError(w, http.StatusUnauthorized, map[string]string{"error": "unknown or revoked dev token"})
			return
		}
		if as != "" {
			local, _, _ := strings.Cut(as, "@")
			s.identityHeaders(w.Header(), Identity{Provider: "dev", Subject: as, Email: as, Name: local}, authHost, host)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	sess := s.session(r, host)
	rule := state.Rule{Level: state.LevelPublic}
	if site.Access != nil {
		rule = site.Access.For(p)
	}
	pass := func() {
		if sess != nil {
			s.identityHeaders(w.Header(), sess.User, authHost, host)
		}
		w.WriteHeader(http.StatusOK)
	}
	switch {
	case rule.Level == state.LevelPublic:
		pass()
	case sess == nil:
		s.denyAnonymous(w, r, method, uri)
	case rule.Level == state.LevelSignedIn || Allowed(sess.User, rule.Allow, groups):
		pass()
	default:
		s.forbidden(w, r, method, sess.User, uri)
	}
}

// cleanPath decodes and normalises the request path the way an app's router
// would see it, so /%61dmin and //admin can't slip past an /admin/* rule.
func cleanPath(uri string) (string, bool) {
	raw, _, _ := strings.Cut(uri, "?")
	if !strings.HasPrefix(raw, "/") {
		return "", false
	}
	p, err := url.PathUnescape(raw)
	if err != nil || strings.ContainsAny(p, "\x00\\") {
		return "", false
	}
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean, true
}

func (s *Server) identityHeaders(h http.Header, id Identity, authHost, host string) {
	h.Set("X-Berth-User-Id", headerSafe(id.ID()))
	h.Set("X-Berth-Email", headerSafe(id.Email))
	// Names can be any Unicode; headers can't, so the name is URL-encoded.
	h.Set("X-Berth-Name", url.PathEscape(id.Name))
	h.Set("X-Berth-Picture", headerSafe(id.Picture))
	h.Set("X-Berth-Identity", identityJWT(s.Key, "https://"+authHost, host, id, time.Now()))
}

func headerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, v)
}

func wantsHTML(r *http.Request, method string) bool {
	return (method == "GET" || method == "HEAD") && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (s *Server) denyAnonymous(w http.ResponseWriter, r *http.Request, method, uri string) {
	login := "/_berth/login?return=" + url.QueryEscape(uri)
	if wantsHTML(r, method) {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, login, http.StatusFound)
		return
	}
	jsonError(w, http.StatusUnauthorized, map[string]string{"error": "sign in required", "login": login})
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, method string, id Identity, uri string) {
	if !wantsHTML(r, method) {
		jsonError(w, http.StatusForbidden, map[string]string{"error": id.Email + " doesn't have access"})
		return
	}
	page(w, http.StatusForbidden, "No access", `<p>You're signed in as <b>{{.Email}}</b>, which doesn't have access to this page.</p>
<p><a class="btn" href="/_berth/login?switch=1&amp;return={{.Return}}">Use another account</a></p>
<form method="post" action="/_berth/logout"><button class="link">Sign out</button></form>`,
		map[string]string{"Email": id.Email, "Return": uri})
}

// Allowed reports whether id matches an allow list: emails, @domain (the
// provider-vouched Workspace domain) or @group.
func Allowed(id Identity, allow []string, groups map[string][]string) bool {
	match := func(w string, expand bool) bool {
		if name, ok := strings.CutPrefix(w, "@"); ok {
			if strings.Contains(name, ".") {
				return id.Domain != "" && strings.EqualFold(id.Domain, name)
			}
			if !expand {
				return false // no groups inside groups
			}
			for _, m := range groups[name] {
				if matchOne(id, m) {
					return true
				}
			}
			return false
		}
		return strings.EqualFold(id.Email, w)
	}
	for _, w := range allow {
		if match(w, true) {
			return true
		}
	}
	return false
}

func matchOne(id Identity, w string) bool {
	if name, ok := strings.CutPrefix(w, "@"); ok {
		return strings.Contains(name, ".") && id.Domain != "" && strings.EqualFold(id.Domain, name)
	}
	return strings.EqualFold(id.Email, w)
}

// ---- sign-in, app side ----

// safeReturn keeps redirects on the same host: a path, never //host.
func safeReturn(ret string) string {
	if ret == "" || !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") || strings.HasPrefix(ret, "/\\") ||
		strings.HasPrefix(ret, "/_berth/") || strings.ContainsAny(ret, "\r\n") {
		return "/"
	}
	return ret
}

func (s *Server) appLogin(w http.ResponseWriter, r *http.Request, host string) {
	p, authHost, _ := s.snapshot()
	if p == nil || authHost == "" {
		page(w, http.StatusServiceUnavailable, "Sign-in isn't set up", `<p>This Berth doesn't have a sign-in provider yet. On the Mac, run <code>berth auth:setup google</code>.</p>`, nil)
		return
	}
	q := r.URL.Query()
	ret := safeReturn(q.Get("return"))
	switchAcct := q.Get("switch") != ""
	if !switchAcct && s.session(r, host) != nil {
		http.Redirect(w, r, ret, http.StatusFound)
		return
	}
	nonce := token()
	n := hashOf(nonce)
	s.mu.Lock()
	full := s.pruneLocked() >= maxPending
	if !full {
		s.logins[n] = &login{host: host, ret: ret, exp: time.Now().Add(loginTTL)}
	}
	s.mu.Unlock()
	if full {
		http.Error(w, "too many sign-ins in progress; try again shortly", http.StatusServiceUnavailable)
		return
	}
	setCookie(w, cookieLogin, nonce, loginTTL)
	u := "https://" + authHost + "/start?host=" + url.QueryEscape(host) + "&n=" + n
	if switchAcct {
		u += "&switch=1"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) appCallback(w http.ResponseWriter, r *http.Request, host string) {
	code := r.URL.Query().Get("code")
	c, err := r.Cookie(cookieLogin)
	n := ""
	if err == nil {
		n = hashOf(c.Value)
	}
	now := time.Now()
	s.mu.Lock()
	h := s.codes[code]
	delete(s.codes, code)
	l := s.logins[n]
	delete(s.logins, n)
	s.mu.Unlock()
	clearCookie(w, cookieLogin)
	if h == nil || l == nil || now.After(h.exp) || now.After(l.exp) || h.host != host || l.host != host || h.n != n {
		page(w, http.StatusBadRequest, "Sign-in expired", `<p>That sign-in link has expired or was opened in another browser.</p><p><a class="btn" href="/_berth/login">Try again</a></p>`, nil)
		return
	}
	setCookie(w, cookieSession, s.Sessions.Create(host, h.user), sessionTTL)
	slog.Info("signed in", "host", host, "email", h.user.Email)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, l.ret, http.StatusFound)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, host string) {
	out := map[string]any{"signed_in": false, "login_url": "/_berth/login", "logout_url": "/_berth/logout"}
	if sess := s.session(r, host); sess != nil {
		out["signed_in"] = true
		out["id"] = sess.User.ID()
		out["email"] = sess.User.Email
		out["name"] = sess.User.Name
		out["picture"] = sess.User.Picture
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// logout ends every session of the signed-in person (all apps and the auth
// host). POST only, from the same origin; GET shows a confirm button.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, host, ret string) {
	if r.Method != "POST" {
		page(w, 200, "Sign out", `<form method="post"><button class="btn">Sign out</button></form>`, nil)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site sign-out refused", http.StatusForbidden)
		return
	}
	if sess := s.session(r, host); sess != nil {
		n := s.Sessions.EndUser(sess.User.ID(), "")
		slog.Info("signed out", "host", host, "email", sess.User.Email, "sessions", n)
	}
	clearCookie(w, cookieSession)
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	sfs := r.Header.Get("Sec-Fetch-Site")
	return sfs == "" || sfs == "same-origin"
}

// ---- sign-in, auth host ----

func (s *Server) authHome(w http.ResponseWriter, r *http.Request, host string) {
	p, _, _ := s.snapshot()
	if sess := s.session(r, host); sess != nil {
		page(w, 200, "Berth sign-in", `<p>Signed in as <b>{{.Email}}</b>{{if .Name}} ({{.Name}}){{end}} with {{.Provider}}.</p>
<form method="post" action="/logout"><button class="btn">Sign out of every app</button></form>`,
			map[string]string{"Email": sess.User.Email, "Name": sess.User.Name, "Provider": sess.User.Provider})
		return
	}
	if p == nil {
		page(w, 200, "Berth sign-in", `<p>Sign-in isn't set up yet: run <code>berth auth:setup google</code> on the Mac.</p>`, nil)
		return
	}
	page(w, 200, "Berth sign-in", `<p>Sign in once here and every Berth app that asks will know who you are.</p><p><a class="btn" href="/start">Sign in with Google</a></p>`, nil)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, authHost string) {
	p, _, _ := s.snapshot()
	if p == nil {
		http.Error(w, "sign-in isn't set up (berth auth:setup google)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	host, n, switchAcct := strings.ToLower(q.Get("host")), q.Get("n"), q.Get("switch") != ""
	if host != "" {
		if _, ok := s.site(host); !ok || !isHex64(n) {
			http.Error(w, "unknown app", http.StatusBadRequest)
			return
		}
		if sess := s.session(r, authHost); sess != nil && !switchAcct {
			s.handOff(w, r, host, n, sess.User)
			return
		}
	}
	st, verifier, nonce := token(), token(), token()
	s.mu.Lock()
	full := s.pruneLocked() >= maxPending
	if !full {
		s.oauth[st] = &oauthState{host: host, n: n, verifier: verifier, nonce: nonce, exp: time.Now().Add(loginTTL)}
	}
	s.mu.Unlock()
	if full {
		http.Error(w, "too many sign-ins in progress; try again shortly", http.StatusServiceUnavailable)
		return
	}
	setCookie(w, cookieOAuth, st, loginTTL)
	sum := sha256.Sum256([]byte(verifier))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, p.AuthURL("https://"+authHost+"/callback", st, nonce, base64.RawURLEncoding.EncodeToString(sum[:]), switchAcct), http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request, authHost string) {
	p, _, _ := s.snapshot()
	q := r.URL.Query()
	st := q.Get("state")
	c, err := r.Cookie(cookieOAuth)
	s.mu.Lock()
	o := s.oauth[st]
	delete(s.oauth, st)
	s.mu.Unlock()
	clearCookie(w, cookieOAuth)
	if e := q.Get("error"); e != "" {
		page(w, http.StatusBadRequest, "Sign-in cancelled", `<p>Google said: {{.}}</p>`, e)
		return
	}
	if p == nil || o == nil || err != nil || c.Value != st || time.Now().After(o.exp) {
		page(w, http.StatusBadRequest, "Sign-in expired", `<p>This sign-in took too long or was started in another browser. Go back to the app and try again.</p>`, nil)
		return
	}
	id, err := p.Exchange(r.Context(), "https://"+authHost+"/callback", q.Get("code"), o.verifier, o.nonce)
	if err != nil {
		slog.Warn("sign-in failed", "err", err)
		page(w, http.StatusBadRequest, "Sign-in failed", `<p>{{.}}</p>`, err.Error())
		return
	}
	setCookie(w, cookieSession, s.Sessions.Create(authHost, *id), sessionTTL)
	if o.host == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.handOff(w, r, o.host, o.n, *id)
}

// handOff sends the browser back to the app with a one-time code bound to
// the app host and the app's nonce cookie.
func (s *Server) handOff(w http.ResponseWriter, r *http.Request, host, n string, id Identity) {
	site, ok := s.site(host)
	if !ok {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	code := token()
	s.mu.Lock()
	s.pruneLocked()
	s.codes[code] = &handoff{host: host, n: n, user: id, exp: time.Now().Add(codeTTL)}
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, site.BaseURL+"/_berth/callback?code="+code, http.StatusFound)
}

// pruneLocked drops expired in-flight sign-ins and returns how many remain.
func (s *Server) pruneLocked() int {
	now := time.Now()
	for k, v := range s.logins {
		if now.After(v.exp) {
			delete(s.logins, k)
		}
	}
	for k, v := range s.oauth {
		if now.After(v.exp) {
			delete(s.oauth, k)
		}
	}
	for k, v := range s.codes {
		if now.After(v.exp) {
			delete(s.codes, k)
		}
	}
	return len(s.logins) + len(s.oauth) + len(s.codes)
}

func isHex64(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

// ---- responses ----

func setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func jsonError(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--card:#fff;--fg:#1d2330;--muted:#5b6475;--accent:#2457d6}
@media (prefers-color-scheme:dark){:root{--bg:#14171c;--card:#1d2128;--fg:#e8ebf0;--muted:#9aa3b2;--accent:#7aa2ff}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,sans-serif;padding:16px;box-sizing:border-box}
main{background:var(--card);max-width:420px;width:100%;padding:28px 28px 20px;border-radius:14px;box-shadow:0 1px 3px #0002}
h1{font-size:20px;margin:0 0 12px}p{margin:0 0 16px;color:var(--muted)}b{color:var(--fg)}
.btn{display:inline-block;background:var(--accent);color:#fff;border:0;border-radius:8px;padding:10px 16px;font:inherit;text-decoration:none;cursor:pointer}
.link{background:none;border:0;color:var(--muted);font:inherit;text-decoration:underline;cursor:pointer;padding:0}
code{font-size:14px}footer{margin-top:20px;font-size:12px;color:var(--muted)}
</style></head><body><main><h1>{{.Title}}</h1>{{.Body}}<footer>Berth sign-in</footer></main></body></html>`))

func page(w http.ResponseWriter, status int, title, body string, data any) {
	var b strings.Builder
	if err := template.Must(template.New("body").Parse(body)).Execute(&b, data); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	pageTmpl.Execute(w, map[string]any{"Title": title, "Body": template.HTML(b.String())})
}
