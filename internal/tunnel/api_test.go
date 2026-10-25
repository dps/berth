package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCF is a minimal Cloudflare API: zones, DNS records, tunnel config and
// the custom-rules ruleset.
type fakeCF struct {
	mu      sync.Mutex
	records []map[string]any
	ingress []Rule
	rules   []map[string]any
	auth    string
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	ok := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/zones" && r.URL.Query().Get("name") == "other.org":
		ok([]map[string]any{{"id": "z1", "name": "other.org", "account": map[string]string{"id": "a1"}}})
	case r.URL.Path == "/zones":
		ok([]any{})
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == "GET":
		var out []map[string]any
		for _, rec := range f.records {
			if rec["name"] == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		ok(out)
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == "POST":
		var rec map[string]any
		json.Unmarshal(body, &rec)
		rec["id"] = "r1"
		f.records = append(f.records, rec)
		ok(rec)
	case strings.HasSuffix(r.URL.Path, "/configurations") && r.Method == "PUT":
		var cfg struct {
			Config struct {
				Ingress []Rule `json:"ingress"`
			} `json:"config"`
		}
		json.Unmarshal(body, &cfg)
		f.ingress = cfg.Config.Ingress
		ok(nil)
	case strings.HasSuffix(r.URL.Path, "/entrypoint") && r.Method == "GET":
		ok(map[string]any{"rules": f.rules})
	case strings.HasSuffix(r.URL.Path, "/entrypoint") && r.Method == "PUT":
		var rs struct {
			Rules []map[string]any `json:"rules"`
		}
		json.Unmarshal(body, &rs)
		f.rules = rs.Rules
		ok(nil)
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "not found: " + r.URL.Path}}})
	}
}

func TestCloudflareAPI(t *testing.T) {
	f := &fakeCF{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := NewAPI("tok")
	api.BaseURL = srv.URL
	ctx := context.Background()

	zone, err := api.ZoneFor(ctx, "www.other.org")
	if err != nil || zone.ID != "z1" {
		t.Fatalf("zone: %v %+v", err, zone)
	}
	if _, err := api.ZoneFor(ctx, "nope.com"); err == nil {
		t.Fatal("expected no zone")
	}
	if err := api.UpsertCNAME(ctx, "z1", "www.other.org", "tid.cfargotunnel.com"); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 || f.records[0]["proxied"] != true || f.records[0]["content"] != "tid.cfargotunnel.com" {
		t.Fatalf("records %+v", f.records)
	}
	rules := Ingress("example.com", "127.0.0.1:7777", "127.0.0.1:7780", []string{"www.other.org"})
	if err := api.PutTunnelConfig(ctx, "a1", "tid", rules); err != nil {
		t.Fatal(err)
	}
	if len(f.ingress) != 4 || f.ingress[1].Hostname != "www.other.org" {
		t.Fatalf("ingress %+v", f.ingress)
	}
	if f.auth != "Bearer tok" {
		t.Fatal("no bearer token")
	}

	// The webhook filter must keep the user's own rules and update ours in place.
	f.rules = []map[string]any{{"id": "u1", "expression": "(ip.src eq 1.2.3.4)", "action": "block"}}
	if err := api.SetWebhookFilter(ctx, "z1", "hooks.example.com", []string{"192.30.252.0/22"}); err != nil {
		t.Fatal(err)
	}
	if len(f.rules) != 2 || f.rules[0]["id"] != "u1" || f.rules[1]["ref"] != filterRuleRef {
		t.Fatalf("rules %+v", f.rules)
	}
	f.rules[1]["id"] = "b1"
	if err := api.SetWebhookFilter(ctx, "z1", "hooks.example.com", []string{"140.82.112.0/20"}); err != nil {
		t.Fatal(err)
	}
	if len(f.rules) != 2 || !strings.Contains(f.rules[1]["expression"].(string), "140.82.112.0/20") || f.rules[1]["id"] != "b1" {
		t.Fatalf("rule not updated in place: %+v", f.rules)
	}
}
