package tools

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is one call through the gateway. Arguments and results are never
// logged.
type Record struct {
	Time     time.Time `json:"t"`
	App      string    `json:"app"`
	Tool     string    `json:"tool"`
	Provider string    `json:"provider,omitempty"` // empty if no provider was reached
	Cached   bool      `json:"cached,omitempty"`
	Ms       int64     `json:"ms"`
	Status   int       `json:"status"`
}

// spent reports whether the record used a provider call.
func (r Record) spent() bool { return r.Provider != "" && !r.Cached }

// Usage appends records to tools/usage-YYYY-MM.jsonl and keeps this
// month's and today's counts in memory, for limits.
type Usage struct {
	dir   string
	mu    sync.Mutex
	month string
	apps  map[string]int64
	days  map[string]map[string]int // day → provider → calls
}

func month(t time.Time) string { return t.Format("2006-01") }
func day(t time.Time) string   { return t.Format("2006-01-02") }

func OpenUsage(dir string) *Usage {
	u := &Usage{dir: dir}
	u.load(month(time.Now()))
	return u
}

func (u *Usage) path(m string) string { return filepath.Join(u.dir, "usage-"+m+".jsonl") }

func (u *Usage) load(m string) {
	u.month, u.apps, u.days = m, map[string]int64{}, map[string]map[string]int{}
	for _, r := range u.read(m) {
		u.count(r)
	}
}

func (u *Usage) count(r Record) {
	if !r.spent() {
		return
	}
	u.apps[r.App]++
	d := day(r.Time)
	if u.days[d] == nil {
		u.days[d] = map[string]int{}
	}
	u.days[d][r.Provider]++
}

func (u *Usage) read(m string) []Record {
	f, err := os.Open(u.path(m))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// roll moves to a new month when one starts. u.mu is held.
func (u *Usage) roll(t time.Time) {
	if m := month(t); m != u.month {
		u.load(m)
	}
}

// Add records a call.
func (u *Usage) Add(r Record) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.roll(r.Time)
	u.count(r)
	os.MkdirAll(u.dir, 0o700)
	f, err := os.OpenFile(u.path(u.month), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	f.Write(append(b, '\n'))
}

// MonthCalls is app's provider calls so far this month.
func (u *Usage) MonthCalls(app string) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.roll(time.Now())
	return u.apps[app]
}

// DayCalls is a provider's calls so far today, across apps.
func (u *Usage) DayCalls(provider string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	u.roll(now)
	return u.days[day(now)][provider]
}

// Row is a summary line for `berth tools:usage`.
type Row struct {
	App      string `json:"app"`
	Tool     string `json:"tool"`
	Provider string `json:"provider"`
	Calls    int    `json:"calls"`
	Cached   int    `json:"cached"`
	Errors   int    `json:"errors"`
	AvgMs    int64  `json:"avg_ms"`
}

// Summary totals a month ("2026-10") by app, tool and provider, optionally
// for one app.
func (u *Usage) Summary(m, app string) []Row {
	rows := map[[3]string]*Row{}
	for _, r := range u.read(m) {
		if app != "" && r.App != app {
			continue
		}
		k := [3]string{r.App, r.Tool, r.Provider}
		row := rows[k]
		if row == nil {
			row = &Row{App: r.App, Tool: r.Tool, Provider: r.Provider}
			rows[k] = row
		}
		row.Calls++
		if r.Cached {
			row.Cached++
		}
		if r.Status >= 400 {
			row.Errors++
		}
		row.AvgMs += r.Ms
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		r.AvgMs /= int64(r.Calls)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.App != b.App {
			return a.App < b.App
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.Provider < b.Provider
	})
	return out
}
