package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dps/berth/internal/analytics"
	"github.com/dps/berth/internal/state"
)

func TestVisitHook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cfg := &state.Config{OnVisit: "blink", OnVisitApps: []string{"site"}}
	var mu sync.Mutex
	var runs [][]string
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	h := newVisitHook(func() (*state.Config, error) { return cfg, nil })
	h.now = func() time.Time { return now }
	h.run = func(ctx context.Context, command string, env []string) error {
		mu.Lock()
		runs = append(runs, env)
		mu.Unlock()
		started <- struct{}{}
		<-release
		return nil
	}
	page := func(path string, ago time.Duration) analytics.Request {
		return analytics.Request{Time: now.Add(-ago), Method: "GET", Host: "berth.computer", Path: path, Status: 200, Browser: "Safari"}
	}

	h.saw("site", page("/", 2*time.Minute))    // an old log line: ignored
	h.saw("other", page("/", 0))               // not one of the apps
	h.saw("site", page("/assets/site.css", 0)) // not a page
	h.saw("site", page("/", 0))
	<-started // the first run is going; these three wait for the next
	h.saw("site", page("/docs/", 0))
	h.saw("site", page("/docs/sqlite/", 0))
	h.saw("site", page("/docs/config/", 0))
	release <- struct{}{}
	<-started
	release <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		running := h.running
		h.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2: %v", len(runs), runs)
	}
	if !slices.Contains(runs[0], "BERTH_VISITS=1") || !slices.Contains(runs[0], "BERTH_PATH=/") {
		t.Errorf("first run: %v", runs[0])
	}
	if !slices.Contains(runs[1], "BERTH_VISITS=3") || !slices.Contains(runs[1], "BERTH_PATH=/docs/config/") || !slices.Contains(runs[1], "BERTH_APP=site") {
		t.Errorf("second run: %v", runs[1])
	}
}

func TestRunVisitCommand(t *testing.T) {
	if err := runVisitCommand(context.Background(), `test "$BERTH_VISITS" = 2`, []string{"BERTH_VISITS=2"}); err != nil {
		t.Fatal(err)
	}
	err := runVisitCommand(context.Background(), `echo no blink1 found >&2; exit 1`, nil)
	if err == nil || err.Error() != "exit status 1: no blink1 found" {
		t.Fatalf("err %v", err)
	}
}
