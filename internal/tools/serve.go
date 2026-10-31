package tools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/dps/berth/internal/state"
)

// AdminEnv is how berthd passes the child its token for tools:call, and
// AIEnv its token for the AI gateway.
const (
	AdminEnv = "BERTH_TOOLS_ADMIN"
	AIEnv    = "BERTH_TOOLS_AI"
)

// sharedKeys are providers whose key may already be saved for the AI
// gateway. It's the same key, so it isn't asked for twice.
var sharedKeys = []string{"openai", "gemini"}

// Keys is every provider key the gateway can use: its own, then any it
// shares with the AI gateway, and aiToken as "ai" if a model is set up.
func Keys(store *state.Store, aiToken string) map[string]string {
	keys := store.ToolKeys()
	shared := store.AIKeys()
	for _, n := range sharedKeys {
		if keys[n] == "" && shared[n] != "" {
			keys[n] = shared[n]
		}
	}
	if aiToken != "" && aiModel(store) != "" {
		keys["ai"] = aiToken
	}
	return keys
}

// Shared reports whether provider's key comes from the AI gateway.
func Shared(store *state.Store, provider string) bool {
	return slices.Contains(sharedKeys, provider) && store.ToolKeys()[provider] == "" && store.AIKeys()[provider] != ""
}

// aiModel is the alias tools ask the AI gateway for: "tools" if the owner
// made one, or "default".
func aiModel(store *state.Store) string {
	cfg, err := store.AIConfig()
	if err != nil {
		return ""
	}
	for _, name := range []string{"tools", "default"} {
		if cfg.Aliases[name] != "" {
			return name
		}
	}
	return ""
}

func sortedNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Serve runs the gateway as `berthd tools`, a child of berthd: dozens of
// integrations parsing other people's data shouldn't be able to take the
// deploy system down with them. It re-reads its setup on SIGHUP and exits
// when berthd does, which closes its stdin.
func Serve(ctx context.Context, store *state.Store) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	g := New(OpenUsage(store.Path("tools")))
	g.SetAdmin(os.Getenv(AdminEnv))
	aiToken := os.Getenv(AIEnv)
	os.Unsetenv(AdminEnv)
	os.Unsetenv(AIEnv)
	reload := func() {
		cfg, err := store.ToolsConfig()
		if err != nil {
			slog.Warn("tools config", "err", err)
			return
		}
		apps, err := store.Apps()
		if err != nil {
			slog.Warn("tools apps", "err", err)
			return
		}
		var list []App
		for _, a := range apps {
			if a.Tools != nil {
				list = append(list, App{Name: a.Name, Policy: *a.Tools})
			}
		}
		g.AIModel = aiModel(store)
		g.Configure(cfg, Keys(store, aiToken), list)
	}
	c, err := store.Config()
	if err != nil {
		return err
	}
	g.BaseURL = fmt.Sprintf("http://%s:%d/tools", c.Gateway, c.AIPort)
	g.AIURL = "http://" + c.AIAddr
	reload()
	go func() {
		for ; ctx.Err() == nil; time.Sleep(time.Hour) {
			g.Files.sweep()
			g.jobs.sweep()
		}
	}()

	ln, err := net.Listen("tcp", c.ToolsAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go func() {
		io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	for {
		select {
		case <-hup:
			reload()
		case <-ctx.Done():
			sctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			return srv.Shutdown(sctx)
		}
	}
}
