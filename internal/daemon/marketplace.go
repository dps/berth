package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dps/berth/internal/tunnel"
)

// ---- eBay marketplace account deletion ----

// eBay keeps a production keyset disabled until it has a public endpoint for
// these notices. Berth holds no eBay user data, so there's nothing to delete:
// it proves it owns the URL and acknowledges each notice. That's also why
// notices aren't signature-checked: a forged one changes nothing here.

func (d *Daemon) marketplaceTokenPath() string {
	return filepath.Join(d.Store.SecretsDir(), "marketplace-token")
}

// marketplaceToken is "" until an owner runs berth tools:ebay-deletion, and
// the endpoint answers 404 until then.
func (d *Daemon) marketplaceToken() string {
	b, err := os.ReadFile(d.marketplaceTokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func marketplaceURL(domain string) string {
	return "https://hooks." + domain + tunnel.MarketplacePath
}

// marketplaceChallenge is the hash eBay asks for when the endpoint is saved.
// The order of the three parts is eBay's.
func marketplaceChallenge(code, token, endpoint string) string {
	sum := sha256.Sum256([]byte(code + token + endpoint))
	return hex.EncodeToString(sum[:])
}

func (d *Daemon) handleMarketplaceChallenge(w http.ResponseWriter, r *http.Request) {
	token := d.marketplaceToken()
	code := r.URL.Query().Get("challenge_code")
	switch {
	case token == "":
		http.NotFound(w, r)
	case code == "":
		http.Error(w, "challenge_code is missing", http.StatusBadRequest)
	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challengeResponse": marketplaceChallenge(code, token, marketplaceURL(d.cfg().Domain))})
	}
}

func (d *Daemon) handleMarketplaceNotice(w http.ResponseWriter, r *http.Request) {
	if d.marketplaceToken() == "" {
		http.NotFound(w, r)
		return
	}
	slog.Info("eBay account deletion notice acknowledged") // the notice itself is personal data
	w.WriteHeader(http.StatusNoContent)
}

// MarketplaceEndpoint is what eBay's Alerts & Notifications page asks for.
type MarketplaceEndpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (d *Daemon) hToolsMarketplace(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	cfg := d.cfg()
	if cfg.NoTunnel || cfg.Domain == "" {
		return nil, errors.New("eBay needs a public URL for this, and this Mac has no tunnel")
	}
	token := d.marketplaceToken()
	if token == "" {
		token = randomHex(32)
		if err := os.MkdirAll(d.Store.SecretsDir(), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(d.marketplaceTokenPath(), []byte(token+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return MarketplaceEndpoint{marketplaceURL(cfg.Domain), token}, nil
}
