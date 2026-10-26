package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dps/berth/internal/github"
	"github.com/dps/berth/internal/state"
)

const (
	maxHookBody    = 1 << 20
	hookRateLimit  = 30 // authenticated deliveries per app per minute
	hookFailLimit  = 10 // failed signatures per source IP per minute
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

// recent counts events for key in the last minute; with add it records
// one more. Keys are "app:<name>" (authenticated deliveries) and
// "fail:<ip>" (bad signatures).
func (d *Daemon) recent(key string, add bool) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	hits := d.hookHits[key][:0]
	for _, t := range d.hookHits[key] {
		if now.Sub(t) < time.Minute {
			hits = append(hits, t)
		}
	}
	if add {
		hits = append(hits, now)
	}
	if len(hits) == 0 {
		delete(d.hookHits, key)
	} else {
		d.hookHits[key] = hits
	}
	return len(hits)
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}

func (d *Daemon) handleGitHub(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	// Senders of bad signatures are limited per IP, before any work, so
	// they can't use up an app's budget for real deliveries.
	if d.recent("fail:"+ip, false) >= hookFailLimit {
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
	// Unknown apps and bad signatures get the same answer, so the endpoint
	// doesn't reveal which app names exist.
	name := r.PathValue("app")
	var app *state.App
	if state.ValidateAppName(name) == nil {
		app, _ = d.Store.App(name)
	}
	if app == nil || app.Webhook.Secret == "" || !github.VerifySignature(app.Webhook.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		if d.recent("fail:"+ip, true) == 1 {
			d.event(state.Event{Kind: "webhook.rejected", App: name, Message: "bad signature from " + ip})
		}
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if d.recent("app:"+name, true) > hookRateLimit {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	if event == "ping" {
		io.WriteString(w, "pong\n")
		return
	}
	if event == "pull_request" {
		if !app.PreviewsOn {
			io.WriteString(w, "previews are off; ignored\n")
			return
		}
		var pe github.PullRequestEvent
		if err := json.Unmarshal(body, &pe); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		d.event(state.Event{Kind: "webhook", App: name, Message: fmt.Sprintf("pull request #%d %s, delivery %s", pe.Number, pe.Action, delivery)})
		go func() {
			if err := d.syncPreviews(d.ctx, name); err != nil {
				slog.Warn("previews", "app", name, "err", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "syncing previews\n")
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
