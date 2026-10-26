package daemon

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dps/berth/internal/auth"
	"github.com/dps/berth/internal/state"
)

// TestAdminPreview serves the live admin page on 127.0.0.1:$BERTH_ADMIN_PREVIEW
// for $BERTH_ADMIN_PREVIEW_SECS seconds, signing requests as a test viewer,
// so the page can be rendered and looked at without a browser sign-in.
// Loopback only; nothing is saved.
func TestAdminPreview(t *testing.T) {
	port := os.Getenv("BERTH_ADMIN_PREVIEW")
	if port == "" {
		t.Skip("BERTH_ADMIN_PREVIEW not set")
	}
	home, _ := os.UserHomeDir()
	s := &state.Store{Root: filepath.Join(home, ".berth")}
	cfg, _ := s.Config()
	ac, _ := s.AuthConfig()
	key, err := auth.LoadKey(filepath.Join(s.SecretsDir(), "auth.key"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://" + cfg.AdminAddr)
	rp := httputil.NewSingleHostReverseProxy(target)
	id := auth.Identity{Provider: "preview", Subject: "1", Email: "preview@localhost", Name: "Preview"}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Berth-Identity", auth.IdentityToken(key, ac.Host, ac.AdminHost, id, time.Now()))
		rp.ServeHTTP(w, r)
	})}
	secs, _ := strconv.Atoi(os.Getenv("BERTH_ADMIN_PREVIEW_SECS"))
	go func() { time.Sleep(time.Duration(max(secs, 30)) * time.Second); srv.Close() }()
	srv.ListenAndServe()
}
