package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dps/berth/internal/state"
)

// A domain added before Berth could reach its zone is attached, with the
// record to create printed; adding it again (after berth cf:login) redoes
// the DNS rather than refusing, and an apex works like any other name.
func TestDomainsAddAgainRedoesDNS(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	cfg, _ := d.Store.Config()
	cfg.TunnelID = "tid"
	d.Store.SaveConfig(cfg)
	d.Store.SaveApp(&state.App{Name: "site", Repo: "o/r", Branch: "main"})
	d.Store.SaveApp(&state.App{Name: "other", Repo: "o/s", Branch: "main"})
	add := func(app, host string) (string, error) {
		var out bytes.Buffer
		p, _ := json.Marshal(DomainParams{App: app, Host: host})
		_, err := d.hDomainsAdd(context.Background(), p, &out)
		return out.String(), err
	}
	out, err := add("site", "berth.computer")
	if err != nil || !strings.Contains(out, "berth cf:login") || !strings.Contains(out, "berth.computer CNAME tid.cfargotunnel.com") {
		t.Fatalf("first add: %v\n%s", err, out)
	}
	if out, err := add("site", "berth.computer"); err != nil || !strings.Contains(out, "CNAME") {
		t.Fatalf("adding it again: %v\n%s", err, out)
	}
	if a, _ := d.Store.App("site"); len(a.Domains) != 1 || a.Domains[0] != "berth.computer" {
		t.Fatalf("domains %v", a.Domains)
	}
	if _, err := add("other", "berth.computer"); err == nil || !strings.Contains(err.Error(), "already attached to site") {
		t.Fatalf("another app: %v", err)
	}
}
