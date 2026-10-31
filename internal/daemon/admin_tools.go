package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tools"
)

// ---- the admin page's Tools section ----
//
// These are the only changes the admin page can make. Each one needs an
// owner, right now, not just a valid sign-in from when the page loaded, and
// a request from the page itself: apps share the admin page's parent domain,
// and could otherwise post to it from a signed-in owner's browser.

const adminWriteMax = 64 << 10

type adminWriteFunc func(r *http.Request, viewer *auth.IdentityClaims, body []byte) (any, error)

func (d *Daemon) adminWrite(h adminWriteFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		viewer, _ := r.Context().Value(viewerKey{}).(*auth.IdentityClaims)
		if viewer == nil || !d.sameOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ac, err := d.Store.AuthConfig()
		if err != nil || !auth.Allowed(auth.Identity{Email: viewer.Email}, []string{"@" + devOwnersGroup}, ac.Groups) {
			http.Error(w, "only owners can change this", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminWriteMax))
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		v, err := h(r, viewer, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, v)
	}
}

// sameOrigin reports whether the request came from the admin page itself.
// A browser always sends Origin on a POST, and scripts can't set it.
func (d *Daemon) sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host || u.Host == d.adminHost(d.cfg())
}

func (d *Daemon) apiTools(w http.ResponseWriter, r *http.Request) {
	st, err := d.hToolsStatus(r.Context(), nil, io.Discard)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

// toolsChanged logs who did what, and returns the new state for the page.
// message must never hold a key.
func (d *Daemon) toolsChanged(r *http.Request, viewer *auth.IdentityClaims, app, message string) (any, error) {
	d.event(state.Event{Kind: "tools.config", App: app, Message: viewer.Email + " " + message})
	return d.hToolsStatus(r.Context(), nil, io.Discard)
}

func (d *Daemon) apiToolsKey(r *http.Request, viewer *auth.IdentityClaims, body []byte) (any, error) {
	var p ToolsKeyParams
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("bad request")
	}
	p.Key = strings.TrimSpace(p.Key)
	if _, err := d.hToolsKey(r.Context(), jsonBody(p), io.Discard); err != nil {
		return nil, err
	}
	what := "changed the daily budget for " + p.Provider
	switch {
	case p.Remove:
		what = "removed the " + p.Provider + " key"
	case p.Key != "":
		what = "saved the " + p.Provider + " key"
	}
	return d.toolsChanged(r, viewer, "", what)
}

func jsonBody(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (d *Daemon) apiToolsUse(r *http.Request, viewer *auth.IdentityClaims, body []byte) (any, error) {
	var p ToolsUseParams
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("bad request")
	}
	if _, err := d.hToolsUse(r.Context(), body, io.Discard); err != nil {
		return nil, err
	}
	order := "the default order"
	if len(p.Providers) > 0 {
		order = strings.Join(p.Providers, ", ")
	}
	return d.toolsChanged(r, viewer, "", "set "+p.Tool+" to "+order)
}

// apiToolsEnable answers once the change is saved. A redeploy can outlast
// the proxies in front of this page, so it runs on its own; limits on an app
// that's already enabled apply at once and need none.
func (d *Daemon) apiToolsEnable(r *http.Request, viewer *auth.IdentityClaims, body []byte) (any, error) {
	var p ToolsEnableParams
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("bad request")
	}
	a, err := d.Store.App(p.App)
	if err != nil {
		return nil, err
	}
	first := a.Tools == nil
	p.NoRestart = true
	if _, err := d.hToolsEnable(r.Context(), jsonBody(p), io.Discard); err != nil {
		return nil, err
	}
	if first {
		d.redeployLater(p.App)
		return d.toolsChanged(r, viewer, p.App, "enabled tools")
	}
	return d.toolsChanged(r, viewer, p.App, "changed tool limits")
}

func (d *Daemon) apiToolsDisable(r *http.Request, viewer *auth.IdentityClaims, body []byte) (any, error) {
	var p ToolsDisableParams
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("bad request")
	}
	p.NoRestart = true
	if _, err := d.hToolsDisable(r.Context(), jsonBody(p), io.Discard); err != nil {
		return nil, err
	}
	d.redeployLater(p.App)
	return d.toolsChanged(r, viewer, p.App, "disabled tools")
}

func (d *Daemon) redeployLater(app string) {
	d.toolsDeploys.Go(func() {
		if _, err := d.redeployForConfig(context.Background(), app, false, false, io.Discard); err != nil {
			slog.Warn("tools: redeploy failed", "app", app, "err", err)
		}
	})
}

// apiToolsTest calls a provider for real, so a key that's wrong shows up
// here rather than in an app. It spends one call.
func (d *Daemon) apiToolsTest(r *http.Request, viewer *auth.IdentityClaims, body []byte) (any, error) {
	var p struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("bad request")
	}
	t, ok := tools.Sample(p.Provider)
	if !ok {
		return nil, fmt.Errorf("%s can't be tested", p.Provider)
	}
	res, err := d.hToolsCall(r.Context(), jsonBody(ToolsCallParams{Tool: t.Name, Args: json.RawMessage(t.Example), Provider: p.Provider}), io.Discard)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"tool": t.Name, "ms": res.(map[string]any)["meta"].(tools.Meta).Ms}
	return out, nil
}
