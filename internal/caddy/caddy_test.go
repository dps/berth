package caddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildRoutes(t *testing.T) {
	cfg := Build(Options{
		Admin: "127.0.0.1:2019", HTTPListen: "127.0.0.1:80", HTTPSListen: ":443", StorageDir: "/tmp/c", DNSAddr: "127.0.0.1:7753", SRVDomain: "berth.internal",
		Routes: []Route{
			{App: "web", PublicHosts: []string{"web.example.com", "www.other.com"}, LANHosts: []string{"web.mini.local"}, Service: "web", AccessLog: "/tmp/web.log"},
			{App: "down", PublicHosts: []string{"down.example.com"}},
			{App: "secret", LANHosts: []string{"secret.mini.local"}, Service: "secret", AuthUser: "u", AuthHash: "$2a$10$x"},
		},
	})
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{
		`"source":"srv"`, `"service":"web"`, `"addresses":["udp/127.0.0.1:7753"]`, `"www.other.com"`, `"status_code":503`, `"status_code":404`,
		`"http_basic"`, `"module":"internal"`, `"install_trust":false`, `"listen":["127.0.0.1:80"]`, `"listen":[":443"]`,
		`"http.log.access.app-web"`, `"Cf-Connecting-Ip"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %s", want)
		}
	}
	// A private app (LAN hosts only) must not appear with a public hostname.
	if strings.Contains(s, "secret.example.com") {
		t.Error("private app got a public host")
	}
	// The 404 catch-all must be the last tunnel route.
	srv := cfg["apps"].(obj)["http"].(obj)["servers"].(obj)["tunnel"].(obj)
	routes := srv["routes"].([]any)
	last, _ := json.Marshal(routes[len(routes)-1])
	if !strings.Contains(string(last), "404") {
		t.Errorf("last route is not the 404: %s", last)
	}
}

func TestBuildNoLAN(t *testing.T) {
	cfg := Build(Options{Admin: "a", HTTPListen: "127.0.0.1:80"})
	b, _ := json.Marshal(cfg)
	if strings.Contains(string(b), `"lan"`) || strings.Contains(string(b), "tls") {
		t.Errorf("LAN server present without HTTPSListen: %s", b)
	}
}

func TestLoad(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/load" || r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	c := NewClient(strings.TrimPrefix(srv.URL, "http://"))
	if err := c.Load(context.Background(), map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if got["x"] != float64(1) {
		t.Fatal(got)
	}
}
