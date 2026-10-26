package analytics

import (
	"os"
	"testing"
	"time"
)

// TestLiveLog loads a real access log (BERTH_LIVE_LOG=path) and times it.
func TestLiveLog(t *testing.T) {
	path := os.Getenv("BERTH_LIVE_LOG")
	if path == "" {
		t.Skip("BERTH_LIVE_LOG not set")
	}
	s := NewStats()
	start := time.Now()
	NewTailer(s, func() map[string][]string { return map[string][]string{"app": {path}} }).Poll()
	load := time.Since(start)
	start = time.Now()
	r := s.Query("app", Keep, time.Hour, 10, true)
	t.Logf("load %s, 7d query %s: %d requests, %d visitors, p50 %.1fms p95 %.1fms, top %v, browsers %v",
		load, time.Since(start), r.Totals.Req, r.Totals.Visitors, r.Totals.P50, r.Totals.P95, r.Paths[:min(3, len(r.Paths))], r.Browsers)
}
