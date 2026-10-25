package services

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

func TestSupervisorSkipsServiceBeingCreated(t *testing.T) {
	s := &state.Store{Root: t.TempDir()}
	cfg := state.DefaultConfig()
	cfg.Gateway = "127.0.0.1"
	s.SaveConfig(cfg)
	rt := runtime.NewFake()
	m := &Manager{Store: s, RT: rt}
	ctx := context.Background()

	// A create in progress: the record exists but the container doesn't yet.
	sv := &state.Service{Kind: "redis", Name: "cache", Image: "redis:7", Port: 16399, Volume: "v", Links: map[string]state.Link{}, CreatedAt: time.Now()}
	s.SaveService(sv)
	l := m.lock("redis", "cache")
	l.Lock()
	if started, err := m.EnsureRunning(ctx, sv, nil); started || err != nil {
		t.Fatalf("supervisor touched a service mid-create: started=%v err=%v", started, err)
	}
	if len(rt.CallLog("run ")) != 0 {
		t.Fatal("container started while the create held the lock")
	}
	l.Unlock()
	if started, err := m.EnsureRunning(ctx, sv, nil); !started || err != nil {
		t.Fatalf("down service not started once free: started=%v err=%v", started, err)
	}
}

func TestCreateAndLink(t *testing.T) {
	s := &state.Store{Root: t.TempDir()}
	cfg := state.DefaultConfig()
	cfg.Gateway = "192.168.66.1"
	s.SaveConfig(cfg)
	m := &Manager{Store: s, RT: runtime.NewFake()}
	ctx := context.Background()
	sv, err := m.Create(ctx, "redis", "cache", "", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "redis", "cache", "", "", io.Discard); err == nil {
		t.Fatal("duplicate create allowed")
	}
	env := state.Env{}
	v, url, err := m.Link(ctx, "redis", "cache", "web", "", env)
	if err != nil || v != "REDIS_URL" || url != "redis://:"+sv.Password+"@192.168.66.1:6001" {
		t.Fatalf("link: %s %s %v", v, url, err)
	}
	// A second Postgres-style variable name when REDIS_URL belongs to another service.
	env["REDIS_URL"] = "redis://:x@192.168.66.1:9999"
	m.Create(ctx, "redis", "sessions", "", "", io.Discard)
	if v, _, _ := m.Link(ctx, "redis", "sessions", "web", "", env); v != "REDIS_SESSIONS_URL" {
		t.Fatalf("second link var %s", v)
	}
}
