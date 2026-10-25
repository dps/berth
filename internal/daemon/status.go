package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dps/berth/internal/deploy"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/internal/tunnel"
)

type AppStatus struct {
	Name        string      `json:"name"`
	State       string      `json:"state"` // running, stopped, crashed, deploying, new
	Release     int         `json:"release,omitempty"`
	SHA         string      `json:"sha,omitempty"`
	Message     string      `json:"message,omitempty"`
	Since       time.Time   `json:"since,omitempty"`
	MemoryBytes int64       `json:"memory_bytes,omitempty"`
	MemoryLimit string      `json:"memory_limit"`
	HostPort    int         `json:"host_port,omitempty"`
	URLs        []string    `json:"urls"`
	Repo        string      `json:"repo,omitempty"`
	Branch      string      `json:"branch,omitempty"`
	Private     bool        `json:"private,omitempty"`
	LastDeploy  *DeployInfo `json:"last_deploy,omitempty"`
	Links       []string    `json:"links,omitempty"`
}

type DeployInfo struct {
	Release int       `json:"release"`
	Status  string    `json:"status"`
	Time    time.Time `json:"time"`
	Error   string    `json:"error,omitempty"`
	Trigger string    `json:"trigger,omitempty"`
}

type ServiceStatus struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Running     bool     `json:"running"`
	Endpoint    string   `json:"endpoint"`
	ExposePort  int      `json:"expose_port,omitempty"`
	Volume      string   `json:"volume"`
	Links       []string `json:"links,omitempty"`
	LastBackup  string   `json:"last_backup,omitempty"`
	MemoryBytes int64    `json:"memory_bytes,omitempty"`
}

type SystemStatus struct {
	Version          string   `json:"version"`
	Uptime           string   `json:"uptime"`
	Domain           string   `json:"domain"`
	ContainerRunning bool     `json:"container_running"`
	CaddyUp          bool     `json:"caddy_up"`
	TunnelMode       string   `json:"tunnel_mode"` // off, local, remote
	TunnelConnected  bool     `json:"tunnel_connected"`
	DiskFreeBytes    uint64   `json:"disk_free_bytes"`
	MemPhysical      int64    `json:"mem_physical"`
	MemCommitted     int64    `json:"mem_committed"`
	FileVault        string   `json:"filevault"`
	Mode             string   `json:"mode"` // filevault | unattended
	Warnings         []string `json:"warnings,omitempty"`
}

type Status struct {
	System   SystemStatus    `json:"system"`
	Apps     []AppStatus     `json:"apps"`
	Services []ServiceStatus `json:"services"`
}

// ParseMemory turns "512M", "1G", "1024" (MiB) into bytes.
func ParseMemory(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := int64(1 << 20)
	switch {
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "KB"):
		mult = 1 << 10
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "MB"):
		mult = 1 << 20
	case strings.HasSuffix(s, "G") || strings.HasSuffix(s, "GB"):
		mult = 1 << 30
	case strings.HasSuffix(s, "T") || strings.HasSuffix(s, "TB"):
		mult = 1 << 40
	}
	n, _ := strconv.ParseFloat(strings.TrimRight(s, "KMGTB"), 64)
	return int64(n * float64(mult))
}

func physicalMemory() int64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// FileVaultStatus returns "on", "off" or "unknown".
func FileVaultStatus() string {
	out, err := exec.Command("fdesetup", "status").Output()
	if err != nil {
		return "unknown"
	}
	s := string(out)
	switch {
	case strings.Contains(s, "FileVault is On"):
		return "on"
	case strings.Contains(s, "FileVault is Off"):
		return "off"
	}
	return "unknown"
}

func (d *Daemon) hStatus(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	return d.Status(ctx)
}

func (d *Daemon) Status(ctx context.Context) (*Status, error) {
	cfg, err := d.Store.Config()
	if err != nil {
		return nil, err
	}
	st := &Status{}
	sys := &st.System
	sys.Version = Version
	sys.Uptime = time.Since(d.started).Round(time.Second).String()
	sys.Domain = cfg.Domain
	sys.ContainerRunning = d.RT.SystemRunning(ctx)
	sys.CaddyUp = d.Caddy != nil && d.Caddy.Ping(ctx) == nil
	switch {
	case cfg.NoTunnel || cfg.TunnelID == "":
		sys.TunnelMode = "off"
	case cfg.TunnelRemote:
		sys.TunnelMode = "remote"
	default:
		sys.TunnelMode = "local"
	}
	if sys.TunnelMode != "off" {
		sys.TunnelConnected = tunnel.Ready(ctx, cfg.TunnelMetrics)
	}
	free, _ := deploy.FreeBytes(d.Store.Root)
	sys.DiskFreeBytes = free
	sys.MemPhysical = physicalMemory()
	sys.FileVault = FileVaultStatus()
	sys.Mode = "filevault"
	if cfg.Unattended {
		sys.Mode = "unattended"
	}

	var cs []runtime.Container
	stats := map[string]runtime.Stat{}
	if sys.ContainerRunning {
		cs, _ = d.RT.List(ctx)
		stats, _ = d.RT.Stats(ctx)
	}
	apps, _ := d.Store.Apps()
	for _, a := range apps {
		env, _ := d.Store.Env(a.Name)
		as := AppStatus{Name: a.Name, URLs: AppURLs(cfg, a), Repo: a.Repo, Branch: a.Branch, Private: a.Private, Links: a.Links,
			MemoryLimit: env.StrSetting(state.KeyMemory, cfg.DefaultMemory)}
		rs, _ := d.Store.Releases(a.Name)
		if len(rs) > 0 {
			r := rs[0]
			t := r.StartedAt
			if t.IsZero() {
				t = r.CreatedAt
			}
			as.LastDeploy = &DeployInfo{Release: r.N, Status: r.Status, Time: t, Error: r.Error, Trigger: r.Trigger}
		}
		cur := d.Store.CurrentRelease(a.Name)
		switch {
		case d.Deployer.Busy(a.Name):
			as.State = "deploying"
		case cur == nil:
			as.State = "new"
		case a.Stopped:
			as.State = "stopped"
		default:
			as.State = "crashed"
			if c, ok := runtime.Find(cs, cur.Container); ok && c.Running() {
				as.State = "running"
				as.Since = c.StartedAt
			}
		}
		if cur != nil {
			as.Release, as.SHA, as.Message, as.HostPort = cur.N, cur.SHA, cur.Message, cur.HostPort
			if s, ok := stats[cur.Container]; ok {
				as.MemoryBytes = s.MemoryBytes
			}
			if as.State == "running" || as.State == "deploying" {
				sys.MemCommitted += ParseMemory(as.MemoryLimit)
			}
		}
		st.Apps = append(st.Apps, as)
	}
	svs, _ := d.Store.Services("")
	for _, sv := range svs {
		name := state.ServiceContainer(sv.Kind, sv.Name)
		ss := ServiceStatus{Kind: sv.Kind, Name: sv.Name, Image: sv.Image, Volume: sv.Volume, ExposePort: sv.ExposePort,
			Endpoint: fmt.Sprintf("%s:%d", cfg.Gateway, sv.Port)}
		if c, ok := runtime.Find(cs, name); ok && c.Running() {
			ss.Running = true
			sys.MemCommitted += ParseMemory(sv.Memory)
		}
		if s, ok := stats[name]; ok {
			ss.MemoryBytes = s.MemoryBytes
		}
		for a := range sv.Links {
			ss.Links = append(ss.Links, a)
		}
		sort.Strings(ss.Links)
		if b := d.Services.LatestBackup(sv.Kind, sv.Name); b != "" {
			ss.LastBackup = b
		}
		st.Services = append(st.Services, ss)
	}
	sys.Warnings = d.warnings(cfg, st)
	return st, nil
}

func (d *Daemon) warnings(cfg *state.Config, st *Status) []string {
	var w []string
	sys := st.System
	if !sys.ContainerRunning {
		w = append(w, "container system is not running (berth doctor)")
	}
	if !sys.CaddyUp {
		w = append(w, "Caddy admin API is not reachable (berth doctor)")
	}
	if sys.TunnelMode != "off" && !sys.TunnelConnected {
		w = append(w, "Cloudflare tunnel is not connected (berth doctor)")
	}
	if sys.DiskFreeBytes > 0 && sys.DiskFreeBytes < 2<<30 {
		w = append(w, fmt.Sprintf("only %s free on disk; deploys are refused below 2 GB (berth gc)", deploy.HumanBytes(int64(sys.DiskFreeBytes))))
	}
	if sys.MemPhysical > 0 && sys.MemCommitted > sys.MemPhysical*8/10 {
		w = append(w, fmt.Sprintf("committed memory %s exceeds 80%% of physical %s", deploy.HumanBytes(sys.MemCommitted), deploy.HumanBytes(sys.MemPhysical)))
	}
	if !cfg.Unattended && sys.FileVault == "on" {
		w = append(w, "FileVault is on: after a power cut the Mac waits for a password before apps return (use a UPS, or see Reboots and FileVault in the docs)")
	}
	for _, a := range st.Apps {
		if a.State == "crashed" {
			w = append(w, fmt.Sprintf("%s is crashed (berth logs %s)", a.Name, a.Name))
		}
	}
	for _, s := range st.Services {
		if !s.Running {
			w = append(w, fmt.Sprintf("%s:%s is not running", s.Kind, s.Name))
		}
	}
	return w
}
