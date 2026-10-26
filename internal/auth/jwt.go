package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// LoadKey reads the Ed25519 signing key, creating it on first use.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		seed, err := base64.StdEncoding.DecodeString(string(b))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("auth.key is corrupt")
		}
		return ed25519.NewKeyFromSeed(seed), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key.Seed())), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

const identityTTL = 5 * time.Minute

// identityJWT is the X-Berth-Identity header: who the visitor is, signed,
// for apps that verify rather than trust the proxy.
func identityJWT(key ed25519.PrivateKey, issuer, audience string, id Identity, now time.Time) string {
	head := b64json(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": keyID(key)})
	claims := map[string]any{
		"iss": issuer, "aud": audience, "sub": id.ID(), "email": id.Email,
		"name": id.Name, "iat": now.Unix(), "exp": now.Add(identityTTL).Unix(),
	}
	signing := head + "." + b64json(claims)
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signing)))
}

func keyID(key ed25519.PrivateKey) string {
	return hashOf(string(key.Public().(ed25519.PublicKey)))[:16]
}

// JWKS is the public key set for verifying X-Berth-Identity.
func JWKS(key ed25519.PrivateKey) []byte {
	b, _ := json.Marshal(map[string]any{"keys": []any{map[string]string{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": keyID(key),
		"x": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
	}}})
	return b
}

func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// IdentityClaims are what VerifyIdentity returns from X-Berth-Identity.
type IdentityClaims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Expires  int64  `json:"exp"`
}

// VerifyIdentity checks an X-Berth-Identity token: signed by key, for
// audience (the host it was issued for), not expired.
func VerifyIdentity(key ed25519.PublicKey, token, audience string, now time.Time) (*IdentityClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed identity token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("bad identity signature")
	}
	var head struct {
		Alg string `json:"alg"`
	}
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if json.Unmarshal(hb, &head) != nil || head.Alg != "EdDSA" {
		return nil, errors.New("unexpected identity algorithm")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c IdentityClaims
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, err
	}
	if c.Audience != audience {
		return nil, fmt.Errorf("identity is for %s, not %s", c.Audience, audience)
	}
	if now.Unix() >= c.Expires {
		return nil, errors.New("identity expired")
	}
	return &c, nil
}

// IdentityToken signs an X-Berth-Identity token, as the sign-in check does
// (exported for tests).
func IdentityToken(key ed25519.PrivateKey, issuer, audience string, id Identity, now time.Time) string {
	return identityJWT(key, issuer, audience, id, now)
}
