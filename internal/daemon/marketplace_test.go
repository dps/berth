package daemon

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dps/berth/internal/tunnel"
)

func TestMarketplaceChallengeHash(t *testing.T) {
	// SHA-256 of "abc", which is what the three parts join to here.
	if got := marketplaceChallenge("a", "b", "c"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got)
	}
}

func TestMarketplaceEndpoint(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	h := d.webhookHandler()
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tunnel.MarketplacePath+query, nil))
		return rec
	}
	post := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", tunnel.MarketplacePath, strings.NewReader(`{"metadata":{"topic":"MARKETPLACE_ACCOUNT_DELETION"}}`)))
		return rec.Code
	}

	if code := get("?challenge_code=x").Code; code != 404 {
		t.Fatalf("answered %d before it was turned on", code)
	}
	if code := post(); code != 404 {
		t.Fatalf("acknowledged %d before it was turned on", code)
	}

	res, err := d.hToolsMarketplace(d.ctx, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	e := res.(MarketplaceEndpoint)
	if e.URL != "https://hooks.example.com/marketplace/account-deletion" || len(e.Token) < 32 || len(e.Token) > 80 {
		t.Fatalf("%+v", e)
	}
	if fi, _ := os.Stat(d.marketplaceTokenPath()); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v", fi)
	}
	if again, _ := d.hToolsMarketplace(d.ctx, nil, io.Discard); again.(MarketplaceEndpoint).Token != e.Token {
		t.Fatal("asking again changed the token, which would break the saved endpoint")
	}

	rec := get("?challenge_code=x")
	var body struct {
		Response string `json:"challengeResponse"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || body.Response != marketplaceChallenge("x", e.Token, e.URL) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if code := get("").Code; code != 400 {
		t.Fatalf("no code: %d", code)
	}
	if code := post(); code != 204 {
		t.Fatalf("notice: %d", code)
	}
}
