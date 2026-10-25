package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/state"
)

const (
	maxHookBody    = 1 << 20
	hookRateLimit  = 30 // requests per app per minute
	keepDeliveries = 50
)

// webhookHandler serves POST /github/<app> on the webhook listener, which is
// reachable from the internet only as hooks.<domain> through the tunnel.
func (d *Daemon) webhookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /github/{app}", d.handleGitHub)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

func (d *Daemon) allowHook(app string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	hits := d.hookHits[app][:0]
	for _, t := range d.hookHits[app] {
		if now.Sub(t) < time.Minute {
			hits = append(hits, t)
		}
	}
	if len(hits) >= hookRateLimit {
		d.hookHits[app] = hits
		return false
	}
	d.hookHits[app] = append(hits, now)
	return true
}

func (d *Daemon) handleGitHub(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if state.ValidateAppName(name) != nil || !d.Store.AppExists(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !d.allowHook(name) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBody+1))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if len(body) > maxHookBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	app, err := d.Store.App(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if app.Webhook.Secret == "" || !github.VerifySignature(app.Webhook.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		d.event(state.Event{Kind: "webhook.rejected", App: name, Message: "bad signature from " + r.Header.Get("Cf-Connecting-Ip")})
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	if event == "ping" {
		io.WriteString(w, "pong\n")
		return
	}
	if event != "push" {
		fmt.Fprintf(w, "ignored event %s\n", event)
		return
	}
	if delivery != "" && slices.Contains(app.Deliveries, delivery) {
		d.event(state.Event{Kind: "webhook", App: name, Message: "duplicate delivery " + delivery + " ignored"})
		io.WriteString(w, "duplicate delivery; ignored\n")
		return
	}
	var push github.PushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if delivery != "" {
		d.updateApp(name, func(a *state.App) {
			a.Deliveries = append(a.Deliveries, delivery)
			if len(a.Deliveries) > keepDeliveries {
				a.Deliveries = a.Deliveries[len(a.Deliveries)-keepDeliveries:]
			}
		})
	}
	branch, isBranch := strings.CutPrefix(push.Ref, "refs/heads/")
	switch {
	case !isBranch:
		io.WriteString(w, "not a branch push; ignored\n")
		return
	case push.Deleted:
		io.WriteString(w, "branch deleted; ignored\n")
		return
	case branch != app.Branch:
		fmt.Fprintf(w, "push to %s, deploying %s; ignored\n", branch, app.Branch)
		return
	case !push.Touches(app.Path):
		d.updateApp(name, func(a *state.App) { a.LastSeen = push.After })
		fmt.Fprintf(w, "no changes under %s; ignored\n", app.Path)
		return
	}
	short := push.After
	if len(short) > 7 {
		short = short[:7]
	}
	d.event(state.Event{Kind: "webhook", App: name, Message: fmt.Sprintf("push to %s (%s), delivery %s", branch, short, delivery)})
	if d.enqueue(name, "webhook", "") {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "deploying %s@%s\n", branch, short)
	} else {
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "deploy already queued\n")
	}
}
