package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/analytics"
	"github.com/dps/berth/internal/state"
)

// The on-visit hook (berth on-visit:set) is a shell command berthd runs
// when someone opens a page: to flash a light on the Mac, ring a bell,
// anything. Page views come from the access logs a second or so after they
// happen. The command runs one at a time; page views that arrive while it's
// running are counted and passed to the next run, so a burst of traffic is
// a few runs, not hundreds.

const (
	visitFresh       = time.Minute      // older page views (a log read at startup) don't run it
	visitHookTimeout = 30 * time.Second // a run that takes longer is killed
	visitConfigAge   = 2 * time.Second  // how long the config is cached between page views
)

type visit struct {
	app string
	r   analytics.Request
}

type visitHook struct {
	config func() (*state.Config, error)
	run    func(ctx context.Context, command string, env []string) error
	now    func() time.Time

	mu      sync.Mutex
	cfg     *state.Config
	cfgAt   time.Time
	running bool
	pending int
	last    visit
	warned  time.Time
}

func newVisitHook(config func() (*state.Config, error)) *visitHook {
	return &visitHook{config: config, run: runVisitCommand, now: time.Now}
}

// saw is the analytics tailer's OnRequest.
func (h *visitHook) saw(app string, r analytics.Request) {
	if !r.PageView() || h.now().Sub(r.Time) > visitFresh {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cmd, _ := h.commandLocked(app); cmd == "" {
		return
	}
	h.pending++
	h.last = visit{app, r}
	if !h.running {
		h.running = true
		go h.loop()
	}
}

// commandLocked is the command to run for a page view of app, or "".
func (h *visitHook) commandLocked(app string) (string, bool) {
	if h.cfg == nil || h.now().Sub(h.cfgAt) > visitConfigAge {
		cfg, err := h.config()
		if err != nil {
			return "", false
		}
		h.cfg, h.cfgAt = cfg, h.now()
	}
	if h.cfg.OnVisit == "" || (len(h.cfg.OnVisitApps) > 0 && !slices.Contains(h.cfg.OnVisitApps, app)) {
		return "", false
	}
	return h.cfg.OnVisit, true
}

func (h *visitHook) loop() {
	for {
		h.mu.Lock()
		n, v := h.pending, h.last
		cmd, _ := h.commandLocked(v.app)
		h.pending = 0
		if n == 0 || cmd == "" {
			h.running = false
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), visitHookTimeout)
		err := h.run(ctx, cmd, visitEnv(v, n))
		cancel()
		if err != nil {
			h.mu.Lock()
			if h.now().Sub(h.warned) > 10*time.Minute {
				slog.Warn("on-visit command failed", "err", err)
				h.warned = h.now()
			}
			h.mu.Unlock()
		}
	}
}

// visitEnv describes the latest page view to the command.
func visitEnv(v visit, n int) []string {
	return []string{
		"BERTH_APP=" + v.app,
		"BERTH_HOST=" + v.r.Host,
		"BERTH_PATH=" + v.r.Path,
		"BERTH_COUNTRY=" + v.r.Country,
		"BERTH_REFERRER=" + v.r.Referrer,
		"BERTH_MOBILE=" + map[bool]string{true: "1", false: "0"}[v.r.Mobile],
		fmt.Sprintf("BERTH_VISITS=%d", n), // page views since the last run
	}
}

func runVisitCommand(ctx context.Context, command string, env []string) error {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	c.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
