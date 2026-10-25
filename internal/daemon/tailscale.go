package daemon

import (
	"log/slog"
	"os/exec"
	"sync"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// tailscaleRegistry advertises each app as a Tailscale Service
// (svc:<app>, reachable as https://<app>.<tailnet>) proxied to Caddy, which
// routes on the Host header like any other hostname. Services must be
// approved once in the Tailscale admin console (or auto-approved by tag).
type tailscaleRegistry struct {
	mu     sync.Mutex
	served map[string]bool
}

var tsReg = &tailscaleRegistry{served: map[string]bool{}}

func (t *tailscaleRegistry) sync(cfg *state.Config, apps []*state.App) {
	if !cfg.Tailscale || cfg.TailnetDomain == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	bin := runtime.LookBin("tailscale")
	want := map[string]bool{}
	for _, a := range apps {
		want[a.Name] = true
		if t.served[a.Name] {
			continue
		}
		out, err := exec.Command(bin, "serve", "--service=svc:"+a.Name, "--https=443", "--bg", "http://"+cfg.CaddyHTTP).CombinedOutput()
		if err != nil {
			slog.Warn("tailscale serve", "app", a.Name, "err", err, "out", string(out))
			continue
		}
		t.served[a.Name] = true
	}
	for name := range t.served {
		if !want[name] {
			exec.Command(bin, "serve", "--service=svc:"+name, "--https=443", "off").Run()
			delete(t.served, name)
		}
	}
}
