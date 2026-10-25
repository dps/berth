// Package auth is Berth's sign-in server: berthd runs it on 127.0.0.1 behind
// Caddy, which asks it about every request to an app with an access policy
// (forward_auth) and proxies /_berth/* and the auth.<domain> host to it.
// See design.md, Sign-in.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Identity is a signed-in person, whatever the provider.
type Identity struct {
	Provider string `json:"provider"`
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Picture  string `json:"picture,omitempty"`
	Domain   string `json:"domain,omitempty"` // Google Workspace domain (hd), vouched for by the provider
}

// ID is stable for the life of the account: "google:<sub>".
func (i Identity) ID() string { return i.Provider + ":" + i.Subject }

// Provider is one OAuth/OIDC login. Adding GitHub means implementing this.
type Provider interface {
	Name() string
	// AuthURL is where the browser goes to sign in. challenge is the PKCE
	// S256 challenge; selectAccount forces the account chooser.
	AuthURL(redirectURI, state, nonce, challenge string, selectAccount bool) string
	// Exchange trades the code for the person's identity, checking nonce.
	Exchange(ctx context.Context, redirectURI, code, verifier, nonce string) (*Identity, error)
}

// Google is OpenID Connect against Google. The endpoints are fields so
// tests can point them at a fake.
type Google struct {
	ClientID     string
	ClientSecret string
	AuthEndpoint string
	TokenURL     string
	HTTP         *http.Client
}

func NewGoogle(clientID, secret string) *Google {
	return &Google{
		ClientID:     clientID,
		ClientSecret: secret,
		AuthEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		HTTP:         &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *Google) Name() string { return "google" }

func (g *Google) AuthURL(redirectURI, state, nonce, challenge string, selectAccount bool) string {
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if selectAccount {
		q.Set("prompt", "select_account")
	}
	return g.AuthEndpoint + "?" + q.Encode()
}

type googleClaims struct {
	Iss           string `json:"iss"`
	Aud           string `json:"aud"`
	Sub           string `json:"sub"`
	Exp           int64  `json:"exp"`
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	HD            string `json:"hd"`
}

func (g *Google) Exchange(ctx context.Context, redirectURI, code, verifier, nonce string) (*Identity, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	json.Unmarshal(body, &tok)
	if resp.StatusCode != 200 || tok.IDToken == "" {
		return nil, fmt.Errorf("google token exchange failed: %s %s %s", resp.Status, tok.Error, tok.Desc)
	}
	// The ID token came straight from Google's token endpoint over TLS, which
	// OIDC accepts in place of checking its signature; the claims are still
	// checked.
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed id_token: %w", err)
	}
	var c googleClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("malformed id_token: %w", err)
	}
	switch {
	case c.Iss != "https://accounts.google.com" && c.Iss != "accounts.google.com":
		return nil, fmt.Errorf("id_token issuer %q", c.Iss)
	case c.Aud != g.ClientID:
		return nil, fmt.Errorf("id_token is for another client")
	case time.Now().Unix() > c.Exp:
		return nil, fmt.Errorf("id_token expired")
	case c.Nonce != nonce:
		return nil, fmt.Errorf("id_token nonce mismatch")
	case c.Sub == "" || c.Email == "":
		return nil, fmt.Errorf("id_token has no subject or email")
	case !c.EmailVerified:
		return nil, fmt.Errorf("Google hasn't verified %s", c.Email)
	}
	return &Identity{Provider: "google", Subject: c.Sub, Email: strings.ToLower(c.Email), Name: c.Name, Picture: c.Picture, Domain: strings.ToLower(c.HD)}, nil
}
