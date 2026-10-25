package ai

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is one request through the gateway. Prompts and replies are never
// logged.
type Record struct {
	Time   time.Time `json:"t"`
	App    string    `json:"app"`
	Asked  string    `json:"asked"` // what the app asked for (alias or provider/model)
	Model  string    `json:"model"` // provider/model that served it
	In     int64     `json:"in"`
	Out    int64     `json:"out"`
	Ms     int64     `json:"ms"`
	Status int       `json:"status"`
}

// Usage appends records to ai/usage-YYYY-MM.jsonl and keeps this month's
// token totals per app in memory, for budgets.
type Usage struct {
	dir    string
	mu     sync.Mutex
	month  string
	totals map[string]int64
}

func month(t time.Time) string { return t.Format("2006-01") }

func OpenUsage(dir string) *Usage {
	u := &Usage{dir: dir}
	u.load(month(time.Now()))
	return u
}

func (u *Usage) path(m string) string { return filepath.Join(u.dir, "usage-"+m+".jsonl") }

func (u *Usage) load(m string) {
	u.month, u.totals = m, map[string]int64{}
	for _, r := range u.read(m) {
		u.totals[r.App] += r.In + r.Out
	}
}

func (u *Usage) read(m string) []Record {
	f, err := os.Open(u.path(m))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// Add records a request.
func (u *Usage) Add(r Record) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if m := month(r.Time); m != u.month {
		u.load(m)
	}
	u.totals[r.App] += r.In + r.Out
	os.MkdirAll(u.dir, 0o700)
	f, err := os.OpenFile(u.path(u.month), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	f.Write(append(b, '\n'))
}

// MonthTokens is app's input+output tokens so far this month.
func (u *Usage) MonthTokens(app string) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if m := month(time.Now()); m != u.month {
		u.load(m)
	}
	return u.totals[app]
}

// Row is a summary line for `berth ai:usage`.
type Row struct {
	App      string `json:"app"`
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	Errors   int    `json:"errors"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
	AvgMs    int64  `json:"avg_ms"`
}

// Summary totals a month ("2026-10") by app and model, optionally for one
// app.
func (u *Usage) Summary(m, app string) []Row {
	rows := map[[2]string]*Row{}
	for _, r := range u.read(m) {
		if app != "" && r.App != app {
			continue
		}
		k := [2]string{r.App, r.Model}
		row := rows[k]
		if row == nil {
			row = &Row{App: r.App, Model: r.Model}
			rows[k] = row
		}
		row.Requests++
		if r.Status >= 400 {
			row.Errors++
		}
		row.In += r.In
		row.Out += r.Out
		row.AvgMs += r.Ms
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		r.AvgMs /= int64(r.Requests)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Model < out[j].Model
	})
	return out
}
