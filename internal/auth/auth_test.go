package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dps/berth/internal/state"
)

// fakeProvider signs in whoever is queued in next, and records the
// redirect URI and PKCE values it was given.
type fakeProvider struct {
	next      Identity
	challenge string
	nonce     string
	select_   bool
	exchanges int
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) AuthURL(redirectURI, state, nonce, challenge string, selectAccount bool) string {
	f.challenge, f.nonce, f.select_ = challenge, nonce, selectAccount
	return "https://idp.example/auth?state=" + url.QueryEscape(state) + "&redirect_uri=" + url.QueryEscape(redirectURI)
}
func (f *fakeProvider) Exchange(ctx context.Context, redirectURI, code, verifier, nonce string) (*Identity, error) {
	f.exchanges++
	sum := sha256sum(verifier)
	if sum != f.challenge {
		return nil, fmt.Errorf("pkce mismatch")
	}
	if nonce != f.nonce || code != "good" {
		return nil, fmt.Errorf("bad code")
	}
	id := f.next
	return &id, nil
}

func sha256sum(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const authHost = "auth.example.com"

func newTestServer(t *testing.T) (*Server, *fakeProvider) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	s := NewServer(OpenSessions(filepath.Join(t.TempDir(), "sessions.json")), key)
	p := &fakeProvider{next: Identity{Provider: "google", Subject: "123", Email: "alice@gmail.com", Name: "Alice Ñ"}}
	s.Configure(p, authHost, map[string][]string{"family": {"bob@gmail.com", "@corp.example"}})
	s.SetSites(map[string]Site{
		"chirp.example.com": {App: "chirp", BaseURL: "https://chirp.example.com", Access: &state.Access{
			Level: state.LevelPublic,
			Paths: []state.PathRule{
				{Path: "/compose/*", Level: state.LevelSignedIn},
				{Path: "/admin/*", Level: state.LevelAllow, Allow: []string{"@family"}},
			},
		}},
		"vault.example.com": {App: "vault", BaseURL: "https://vault.example.com", Access: &state.Access{Level: state.LevelAllow, Allow: []string{"alice@gmail.com"}}},
	})
	return s, p
}

// browser keeps cookies per host, like a real one (host-only cookies).
type browser struct {
	t       *testing.T
	s       *Server
	cookies map[string]map[string]string
}

func newBrowser(t *testing.T, s *Server) *browser {
	return &browser{t: t, s: s, cookies: map[string]map[string]string{}}
}

func (b *browser) do(method, rawURL string, hdr map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	u, _ := url.Parse(rawURL)
	r := httptest.NewRequest(method, u.RequestURI(), nil)
	r.Host = u.Host
	r.Header.Set("Accept", "text/html")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for name, v := range b.cookies[u.Host] {
		r.AddCookie(&http.Cookie{Name: name, Value: v})
	}
	w := httptest.NewRecorder()
	b.s.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if b.cookies[u.Host] == nil {
			b.cookies[u.Host] = map[string]string{}
		}
		if c.MaxAge < 0 {
			delete(b.cookies[u.Host], c.Name)
		} else {
			b.cookies[u.Host][c.Name] = c.Value
		}
	}
	return w
}

// check is Caddy's forward_auth call for a request to host+uri.
func (b *browser) check(host, uri string) *httptest.ResponseRecorder {
	return b.do("GET", "https://"+host+"/_berth/check", map[string]string{"X-Forwarded-Uri": uri, "X-Forwarded-Method": "GET"})
}

func location(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want redirect: %s", w.Code, w.Body)
	}
	return w.Header().Get("Location")
}

// signIn runs the whole flow from an app's /_berth/login and returns the
// final redirect.
func (b *browser) signIn(host, ret string) string {
	t := b.t
	t.Helper()
	start := location(t, b.do("GET", "https://"+host+"/_berth/login?return="+url.QueryEscape(ret), nil))
	if !strings.HasPrefix(start, "https://"+authHost+"/start?") {
		t.Fatalf("login went to %s", start)
	}
	next := location(t, b.do("GET", start, nil))
	if strings.HasPrefix(next, "https://idp.example/") {
		st := mustQuery(t, next, "state")
		next = location(t, b.do("GET", "https://"+authHost+"/callback?code=good&state="+url.QueryEscape(st), nil))
	}
	if !strings.HasPrefix(next, "https://"+host+"/_berth/callback?code=") {
		t.Fatalf("hand-off went to %s", next)
	}
	return location(t, b.do("GET", next, nil))
}

func mustQuery(t *testing.T, raw, key string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}

func TestPublicPathPassesAnonymous(t *testing.T) {
	s, _ := newTestServer(t)
	b := newBrowser(t, s)
	w := b.check("chirp.example.com", "/")
	if w.Code != 200 || w.Header().Get("X-Berth-Email") != "" {
		t.Fatalf("anonymous public: %d %v", w.Code, w.Header())
	}
}

func TestSignInFlowAndIdentityHeaders(t *testing.T) {
	s, p := newTestServer(t)
	b := newBrowser(t, s)
	w := b.check("chirp.example.com", "/compose/new?x=1")
	if got := location(t, w); got != "/_berth/login?return="+url.QueryEscape("/compose/new?x=1") {
		t.Fatalf("redirect %q", got)
	}
	if final := b.signIn("chirp.example.com", "/compose/new?x=1"); final != "/compose/new?x=1" {
		t.Fatalf("landed on %q", final)
	}
	w = b.check("chirp.example.com", "/compose/new")
	if w.Code != 200 {
		t.Fatalf("signed-in check: %d", w.Code)
	}
	h := w.Header()
	if h.Get("X-Berth-Email") != "alice@gmail.com" || h.Get("X-Berth-User-Id") != "google:123" {
		t.Fatalf("headers %v", h)
	}
	if n, _ := url.PathUnescape(h.Get("X-Berth-Name")); n != "Alice Ñ" {
		t.Fatalf("name %q", h.Get("X-Berth-Name"))
	}
	// The identity JWT verifies with the published key.
	parts := strings.Split(h.Get("X-Berth-Identity"), ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !ed25519.Verify(s.Key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("identity JWT signature")
	}
	var claims map[string]any
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(payload, &claims)
	if claims["aud"] != "chirp.example.com" || claims["sub"] != "google:123" {
		t.Fatalf("claims %v", claims)
	}
	// The session cookie is host-only: another app doesn't see it...
	if w := b.check("vault.example.com", "/"); w.Code != http.StatusFound {
		t.Fatalf("vault without its own session: %d", w.Code)
	}
	// ...but signing in there reuses the auth host session (no IdP trip).
	before := p.exchanges
	b.signIn("vault.example.com", "/")
	if p.exchanges != before {
		t.Fatal("second app went back to the provider")
	}
	if w := b.check("vault.example.com", "/"); w.Code != 200 {
		t.Fatalf("vault after SSO: %d", w.Code)
	}
}

func TestAllowListsAndGroups(t *testing.T) {
	s, p := newTestServer(t)
	alice := newBrowser(t, s)
	alice.signIn("chirp.example.com", "/")
	w := alice.check("chirp.example.com", "/admin/")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "alice@gmail.com") {
		t.Fatalf("alice on /admin: %d", w.Code)
	}
	if w := alice.check("chirp.example.com", "/%61dmin/x"); w.Code != http.StatusForbidden {
		t.Fatalf("encoded path slipped past the rule: %d", w.Code)
	}
	if w := alice.check("chirp.example.com", "//admin/x"); w.Code != http.StatusForbidden {
		t.Fatalf("double slash slipped past the rule: %d", w.Code)
	}
	if w := alice.check("chirp.example.com", "/admin"); w.Code != http.StatusForbidden {
		t.Fatalf("/admin without slash: %d", w.Code)
	}
	p.next = Identity{Provider: "google", Subject: "456", Email: "bob@gmail.com"}
	bob := newBrowser(t, s)
	bob.signIn("chirp.example.com", "/")
	if w := bob.check("chirp.example.com", "/admin/x"); w.Code != 200 {
		t.Fatalf("bob (in @family) on /admin: %d", w.Code)
	}
	p.next = Identity{Provider: "google", Subject: "789", Email: "carol@corp.example", Domain: "corp.example"}
	carol := newBrowser(t, s)
	carol.signIn("chirp.example.com", "/")
	if w := carol.check("chirp.example.com", "/admin/x"); w.Code != 200 {
		t.Fatalf("carol (@corp.example via group) on /admin: %d", w.Code)
	}
	// A consumer account whose email merely ends in the domain doesn't match.
	if Allowed(Identity{Email: "mallory@corp.example"}, []string{"@corp.example"}, nil) {
		t.Fatal("email suffix matched a Workspace domain rule")
	}
	// Groups changes apply on the next request.
	s.Configure(p, authHost, map[string][]string{})
	if w := bob.check("chirp.example.com", "/admin/x"); w.Code != http.StatusForbidden {
		t.Fatalf("bob after leaving the group: %d", w.Code)
	}
}

func TestAPIClientsGet401(t *testing.T) {
	s, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/_berth/check", nil)
	r.Host = "vault.example.com"
	r.Header.Set("X-Forwarded-Uri", "/api/x")
	r.Header.Set("X-Forwarded-Method", "POST")
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST without session: %d", w.Code)
	}
}

func TestHandOffCodeIsBoundToBrowserAndSingleUse(t *testing.T) {
	s, _ := newTestServer(t)
	victim := newBrowser(t, s)
	attacker := newBrowser(t, s)
	// The attacker signs in and stops before redeeming the code, then sends
	// the callback link to the victim (login CSRF).
	start := location(t, attacker.do("GET", "https://chirp.example.com/_berth/login", nil))
	idp := location(t, attacker.do("GET", start, nil))
	cb := location(t, attacker.do("GET", "https://"+authHost+"/callback?code=good&state="+url.QueryEscape(mustQuery(t, idp, "state")), nil))
	if w := victim.do("GET", cb, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("victim redeemed the attacker's code: %d", w.Code)
	}
	// The code was burned by that attempt, so the attacker can't use it either.
	if w := attacker.do("GET", cb, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("code reused: %d", w.Code)
	}
}

func TestOAuthStateMustMatchCookie(t *testing.T) {
	s, _ := newTestServer(t)
	a, other := newBrowser(t, s), newBrowser(t, s)
	start := location(t, a.do("GET", "https://chirp.example.com/_berth/login", nil))
	idp := location(t, a.do("GET", start, nil))
	w := other.do("GET", "https://"+authHost+"/callback?code=good&state="+url.QueryEscape(mustQuery(t, idp, "state")), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("callback in another browser: %d", w.Code)
	}
}

func TestStartRefusesUnknownHosts(t *testing.T) {
	s, _ := newTestServer(t)
	b := newBrowser(t, s)
	w := b.do("GET", "https://"+authHost+"/start?host=evil.example&n="+hashOf("x"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("start for unknown host: %d", w.Code)
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/x?y=1": "/x?y=1", "": "/", "//evil.com": "/", "/\\evil.com": "/", "https://evil.com": "/",
		"/_berth/logout": "/", "/a\r\nSet-Cookie: x": "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogoutNeedsSameOriginAndEndsEverySession(t *testing.T) {
	s, _ := newTestServer(t)
	b := newBrowser(t, s)
	b.signIn("chirp.example.com", "/")
	b.signIn("vault.example.com", "/")
	if w := b.do("POST", "https://chirp.example.com/_berth/logout", map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout: %d", w.Code)
	}
	if w := b.do("POST", "https://chirp.example.com/_berth/logout", map[string]string{"Origin": "https://chirp.example.com"}); w.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := b.check("vault.example.com", "/"); w.Code != http.StatusFound {
		t.Fatalf("vault still signed in after logout: %d", w.Code)
	}
	if n := len(s.Sessions.List()); n != 0 {
		t.Fatalf("%d sessions left", n)
	}
}

func TestSessionsPersistAndExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s := OpenSessions(path)
	v := s.Create("a.example", Identity{Provider: "google", Subject: "1", Email: "a@b.co"})
	if OpenSessions(path).Get("a.example", v) == nil {
		t.Fatal("session lost across restart")
	}
	if s.Get("b.example", v) != nil {
		t.Fatal("session valid on another host")
	}
	s.mu.Lock()
	for _, x := range s.m {
		x.Expires = time.Now().Add(-time.Second)
	}
	s.mu.Unlock()
	if s.Get("a.example", v) != nil {
		t.Fatal("expired session accepted")
	}
	if n := s.EndUser("", "a@b.co"); n != 0 {
		t.Fatalf("ended %d", n)
	}
}

func TestLoginWithoutProvider(t *testing.T) {
	s, _ := newTestServer(t)
	s.Configure(nil, "", nil)
	b := newBrowser(t, s)
	if w := b.do("GET", "https://chirp.example.com/_berth/login", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login with no provider: %d", w.Code)
	}
}

func TestGoogleExchangeChecksClaims(t *testing.T) {
	claims := map[string]any{"iss": "https://accounts.google.com", "aud": "cid", "sub": "42", "exp": time.Now().Add(time.Hour).Unix(),
		"nonce": "n1", "email": "Alice@Gmail.com", "email_verified": true, "name": "Alice"}
	var gotForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = r.PostForm
		tok := "x." + b64json(claims) + ".sig"
		json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
	}))
	defer ts.Close()
	g := NewGoogle("cid", "secret")
	g.TokenURL = ts.URL
	id, err := g.Exchange(context.Background(), "https://auth.x/callback", "c", "v", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "alice@gmail.com" || id.ID() != "google:42" || gotForm.Get("code_verifier") != "v" || gotForm.Get("client_secret") != "secret" {
		t.Fatalf("id %+v form %v", id, gotForm)
	}
	for name, mut := range map[string]func(){
		"nonce":    func() { claims["nonce"] = "other" },
		"aud":      func() { claims["aud"] = "someone-else" },
		"verified": func() { claims["email_verified"] = false },
		"expired":  func() { claims["exp"] = time.Now().Add(-time.Minute).Unix() },
		"issuer":   func() { claims["iss"] = "https://evil.example" },
	} {
		saved := map[string]any{}
		for k, v := range claims {
			saved[k] = v
		}
		mut()
		if _, err := g.Exchange(context.Background(), "https://auth.x/callback", "c", "v", "n1"); err == nil {
			t.Errorf("%s: accepted a bad id_token", name)
		}
		claims = saved
	}
	u, _ := url.Parse(g.AuthURL("https://auth.x/callback", "st", "no", "ch", false))
	if q := u.Query(); q.Get("scope") != "openid email profile" || q.Get("code_challenge_method") != "S256" || q.Get("prompt") != "" {
		t.Fatalf("auth url %s", u)
	}
}

func TestAccessFor(t *testing.T) {
	a := &state.Access{Level: state.LevelPublic, Paths: []state.PathRule{
		{Path: "/a/*", Level: state.LevelSignedIn},
		{Path: "/a/b/*", Level: state.LevelAllow, Allow: []string{"x@y.co"}},
		{Path: "/exact", Level: state.LevelSignedIn},
	}}
	for p, want := range map[string]string{
		"/": "public", "/a": "signed-in", "/a/": "signed-in", "/a/x": "signed-in", "/a/b/c": "allow",
		"/ab": "public", "/exact": "signed-in", "/exact/x": "public",
	} {
		if got := a.For(p).Level; got != want {
			t.Errorf("For(%q) = %s, want %s", p, got, want)
		}
	}
}
