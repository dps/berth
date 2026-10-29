package daemon

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseIostatCPU(t *testing.T) {
	out := "      cpu    load average\n us sy id   1m   5m   15m\n  6  3 92  1.95 1.94 1.88\n 10  9 81  1.95 1.94 1.88\n"
	if got := parseIostatCPU(out); got < 0.189 || got > 0.191 {
		t.Fatalf("cpu = %v, want 0.19 (from the second report)", got)
	}
	if got := parseIostatCPU("nonsense"); got != -1 {
		t.Fatalf("cpu = %v, want -1", got)
	}
}

func TestLoadAppCPU(t *testing.T) {
	var m loadMonitor
	t0 := time.Unix(1_000_000, 0)
	owner := map[string]string{"berth-air-3": "air", "berth-air-3-worker": "air", "pg-main": "pg:main"}
	if got := m.appCPU(t0, map[string]int64{"berth-air-3": 5e6}, owner); got != nil {
		t.Fatalf("first call = %v, want nil (nothing to compare with)", got)
	}
	got := m.appCPU(t0.Add(10*time.Second), map[string]int64{
		"berth-air-3":        10e6, // 5 s of CPU in 10 s: half a core
		"berth-air-3-worker": 1e6,  // new: left out
		"pg-main":            0,
		"stranger":           99e6,
	}, owner)
	if got["air"] != 0.5 || len(got) != 1 {
		t.Fatalf("apps = %v, want air 0.5 only", got)
	}
	// A restarted container's counter goes down: skipped, not negative.
	got = m.appCPU(t0.Add(20*time.Second), map[string]int64{"berth-air-3": 1e6, "berth-air-3-worker": 3e6}, owner)
	if got["air"] != 0.2 {
		t.Fatalf("apps = %v, want air 0.2 (the worker only)", got)
	}
}

func TestLoadQueryAndPersist(t *testing.T) {
	var m loadMonitor
	now := time.Unix(1_000_000_020, 0) // on the minute
	old := now.Add(-25 * time.Hour)
	m.add(loadSample{T: old.Unix(), hostLoad: hostLoad{Load: [3]float64{9, 9, 9}}})
	for i := range 4 {
		ts := now.Add(-time.Duration(i) * 15 * time.Second).Unix()
		m.add(loadSample{T: ts, hostLoad: hostLoad{Load: [3]float64{float64(i), 1, 1}, CPU: 0.5, MemPressure: 0.4}, Apps: map[string]float64{"air": 0.1}})
	}
	m.add(loadSample{T: now.Add(time.Second).Unix(), hostLoad: hostLoad{Load: [3]float64{2, 1, 1}, CPU: -1}})
	m.mu.Lock()
	n := len(m.samples)
	m.mu.Unlock()
	if n != 5 {
		t.Fatalf("kept %d samples, want 5 (the 25-hour-old one dropped)", n)
	}
	rep := m.query(now.Add(time.Second), time.Hour, time.Minute)
	if len(rep.Points) != 60 {
		t.Fatalf("%d points, want 60", len(rep.Points))
	}
	last, prev := rep.Points[59], rep.Points[58]
	// now's minute: samples at :00 (load 0) and :01 (load 2, CPU unknown)
	if last.N != 2 || last.Load[0] != 1 || last.CPU != 0.5 {
		t.Fatalf("last point = %+v", last)
	}
	if prev.N != 3 || prev.Apps["air"] < 0.0999 || prev.Apps["air"] > 0.1001 {
		t.Fatalf("previous point = %+v", prev)
	}
	if rep.Points[0].N != 0 || rep.Points[0].CPU != -1 {
		t.Fatalf("empty point = %+v", rep.Points[0])
	}
	if rep.Peak == nil || rep.Peak.Load[0] != 3 || rep.Latest.Load[0] != 2 {
		t.Fatalf("peak %+v latest %+v", rep.Peak, rep.Latest)
	}

	path := filepath.Join(t.TempDir(), "load.json")
	if err := m.save(path); err != nil {
		t.Fatal(err)
	}
	var back loadMonitor
	back.restore(path, now.Add(24*time.Hour-10*time.Second)) // three samples now too old
	if len(back.samples) != 2 {
		t.Fatalf("restored %d samples, want 2", len(back.samples))
	}
}
