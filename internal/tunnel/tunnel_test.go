package tunnel

import (
	"strings"
	"testing"
)

func TestIngressOrder(t *testing.T) {
	rules := Ingress("example.com", "127.0.0.1:7777", "127.0.0.1:80", []string{"www.b.com", "a.org"})
	want := []string{"hooks.example.com", "a.org", "www.b.com", "*.example.com", ""}
	if len(rules) != len(want) {
		t.Fatalf("%+v", rules)
	}
	for i, w := range want {
		if rules[i].Hostname != w {
			t.Errorf("rule %d = %q, want %q", i, rules[i].Hostname, w)
		}
	}
	if rules[0].Service != "http://127.0.0.1:7777" || rules[len(rules)-1].Service != "http_status:404" {
		t.Error("services wrong")
	}
	yml := LocalConfig("id", "/c.json", rules)
	if !strings.Contains(yml, `hostname: "*.example.com"`) || !strings.Contains(yml, "service: http_status:404") {
		t.Error(yml)
	}
}

func TestFilterExpression(t *testing.T) {
	e := FilterExpression("hooks.x.com", []string{"192.30.252.0/22", "2a0a:a440::/29"})
	if e != `(http.host eq "hooks.x.com" and not ip.src in {192.30.252.0/22 2a0a:a440::/29})` {
		t.Fatal(e)
	}
}
