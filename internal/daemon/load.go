package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dps/berth/internal/state"
)

// ---- system load (design.md, Admin page) ----
//
// berthd samples the Mac's load every loadEvery and keeps the last
// loadKeep in memory, saved to load.json now and then so an upgrade or
// restart doesn't lose the history. Each sample also has the CPU every app
// and service used since the one before, from its containers' CPU time.

const (
	loadEvery = 15 * time.Second
	loadKeep  = 24 * time.Hour
	loadSave  = 5 * time.Minute
)

type hostLoad struct {
	Cores       int        `json:"cores,omitempty"`
	Load        [3]float64 `json:"load"`         // 1, 5 and 15 minute averages
	CPU         float64    `json:"cpu"`          // 0–1 busy, or -1 when unknown
	MemPressure float64    `json:"mem_pressure"` // 0–1
	SwapUsed    int64      `json:"swap_used"`
	SwapTotal   int64      `json:"swap_total"`
}

type loadSample struct {
	T int64 `json:"t"` // Unix seconds
	hostLoad
	// Cores used by each app (all its containers) and service ("pg:main").
	Apps map[string]float64 `json:"apps,omitempty"`
}

type loadMonitor struct {
	mu      sync.Mutex
	samples []loadSample // oldest first
	cpu     map[string]int64
	cpuAt   time.Time
}

// add appends a sample and drops those older than loadKeep.
func (m *loadMonitor) add(s loadSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples = append(m.samples, s)
	cut := s.T - int64(loadKeep/time.Second)
	i := 0
	for i < len(m.samples) && m.samples[i].T < cut {
		i++
	}
	if i > 0 {
		m.samples = append(m.samples[:0:0], m.samples[i:]...)
	}
}

// appCPU turns containers' cumulative CPU time into cores used per owner
// since the last call. A container that restarted (its counter went
// down) or is new is left out until the next call.
func (m *loadMonitor) appCPU(now time.Time, usec map[string]int64, owner map[string]string) map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out map[string]float64
	if secs := now.Sub(m.cpuAt).Seconds(); m.cpu != nil && secs > 0 {
		out = map[string]float64{}
		for c, cur := range usec {
			o, ok := owner[c]
			prev, seen := m.cpu[c]
			if !ok || !seen || cur < prev {
				continue
			}
			out[o] += float64(cur-prev) / 1e6 / secs
		}
	}
	m.cpu, m.cpuAt = usec, now
	return out
}

// LoadPoint is the average of the samples in one step of a LoadReport.
// N is how many there were; 0 means none (berthd wasn't running).
type LoadPoint struct {
	T           int64              `json:"t"`
	N           int                `json:"n"`
	Load        [3]float64         `json:"load"`
	CPU         float64            `json:"cpu"` // -1 when unknown
	MemPressure float64            `json:"mem_pressure"`
	SwapUsed    int64              `json:"swap_used"`
	Apps        map[string]float64 `json:"apps,omitempty"`
}

type LoadReport struct {
	Step   int64       `json:"step"` // seconds
	Latest *loadSample `json:"latest"`
	Points []LoadPoint `json:"points"`          // oldest first
	Peak   *loadSample `json:"peak,omitempty"`  // highest 1-minute load in the range
	Since  int64       `json:"since,omitempty"` // the oldest sample kept
}

// query averages the samples in the last rng into steps of step.
func (m *loadMonitor) query(now time.Time, rng, step time.Duration) *LoadReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := int64(step / time.Second)
	end := now.Unix()/st*st + st
	start := end - int64(rng/time.Second)
	n := int(int64(rng/time.Second) / st)
	rep := &LoadReport{Step: st, Points: make([]LoadPoint, n)}
	cpuN := make([]int, n)
	for i := range rep.Points {
		rep.Points[i].T = start + int64(i)*st
	}
	if len(m.samples) > 0 {
		last := m.samples[len(m.samples)-1]
		rep.Latest, rep.Since = &last, m.samples[0].T
	}
	for _, s := range m.samples {
		i := int((s.T - start) / st)
		if s.T < start || i >= n {
			continue
		}
		if rep.Peak == nil || s.Load[0] > rep.Peak.Load[0] {
			peak := s
			rep.Peak = &peak
		}
		p := &rep.Points[i]
		p.N++
		for k := range 3 {
			p.Load[k] += s.Load[k]
		}
		if s.CPU >= 0 {
			p.CPU += s.CPU
			cpuN[i]++
		}
		p.MemPressure += s.MemPressure
		p.SwapUsed += s.SwapUsed
		for a, v := range s.Apps {
			if p.Apps == nil {
				p.Apps = map[string]float64{}
			}
			p.Apps[a] += v
		}
	}
	for i := range rep.Points {
		p := &rep.Points[i]
		if p.N == 0 {
			p.CPU = -1
			continue
		}
		f := float64(p.N)
		for k := range 3 {
			p.Load[k] /= f
		}
		if cpuN[i] > 0 {
			p.CPU /= float64(cpuN[i])
		} else {
			p.CPU = -1
		}
		p.MemPressure /= f
		p.SwapUsed /= int64(p.N)
		for a := range p.Apps {
			p.Apps[a] /= f
		}
	}
	return rep
}

func (m *loadMonitor) save(path string) error {
	m.mu.Lock()
	b, err := json.Marshal(m.samples)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return state.WriteFile(path, b, 0o600)
}

func (m *loadMonitor) restore(path string, now time.Time) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var ss []loadSample
	if json.Unmarshal(b, &ss) != nil {
		return
	}
	cut := now.Add(-loadKeep).Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range ss {
		if s.T >= cut && s.T <= now.Unix() {
			m.samples = append(m.samples, s)
		}
	}
}

// startLoad samples the Mac's load until ctx is done. Run saves the
// samples on the way out (saveLoad).
func (d *Daemon) startLoad(ctx context.Context) {
	d.load.restore(d.Store.Path("load.json"), time.Now())
	go func() {
		tick := time.NewTicker(loadEvery)
		defer tick.Stop()
		save := time.NewTicker(loadSave)
		defer save.Stop()
		d.sampleLoad(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-save.C:
				d.saveLoad()
			case <-tick.C:
				d.sampleLoad(ctx)
			}
		}
	}()
}

func (d *Daemon) saveLoad() {
	if err := d.load.save(d.Store.Path("load.json")); err != nil {
		slog.Warn("load: save", "err", err)
	}
}

func (d *Daemon) sampleLoad(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s := loadSample{hostLoad: readHostLoad(ctx)}
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	s.T = now.Unix()
	if stats, err := d.RT.Stats(ctx); err == nil {
		usec := map[string]int64{}
		for c, st := range stats {
			usec[c] = st.CPUUsec
		}
		s.Apps = d.load.appCPU(now, usec, d.containerOwners())
	}
	d.load.add(s)
}

// containerOwners maps each running container berthd knows to its app or
// service ("pg:main").
func (d *Daemon) containerOwners() map[string]string {
	owner := map[string]string{}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		cur := d.Store.CurrentRelease(a.Name)
		if cur == nil {
			continue
		}
		if cur.Container != "" {
			owner[cur.Container] = a.Name
		}
		for _, w := range cur.Workers {
			owner[w.Container] = a.Name
		}
	}
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		owner[state.ServiceContainer(sv.Kind, sv.Name)] = sv.Kind + ":" + sv.Name
	}
	return owner
}

// parseIostatCPU returns 1 - idle from iostat's last report, or -1.
//
//	     cpu    load average
//	us sy id   1m   5m   15m
//	 6  3 92  1.95 1.94 1.88
func parseIostatCPU(out string) float64 {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		return -1
	}
	head := strings.Fields(lines[1])
	last := strings.Fields(lines[len(lines)-1])
	for i, f := range head {
		if f == "id" && i < len(last) {
			if idle, err := strconv.ParseFloat(last[i], 64); err == nil {
				return min(1, max(0, (100-idle)/100))
			}
		}
	}
	return -1
}
