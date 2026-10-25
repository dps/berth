// Package runtime wraps Apple's `container` CLI. Every call is one
// shell-out, using --format json where the CLI offers it. A fake
// implementation (fake.go) backs the tests on Linux.
package runtime

import (
	"context"
	"io"
	"time"
)

type BuildOpts struct {
	Tag        string
	Dockerfile string
	Context    string
	BuildArgs  []string // KEY=value
	NoCache    bool
	Out        io.Writer
}

type RunOpts struct {
	Name        string
	Image       string
	Network     string
	Publish     []string // [host-ip:]host-port:container-port
	EnvFile     string
	Env         []string // KEY=value
	CPUs        string
	Memory      string
	Volumes     []string // source:target
	Labels      map[string]string
	Init        bool
	Remove      bool // --rm
	Detach      bool
	Entrypoint  string
	Args        []string
	Interactive bool
	TTY         bool
}

type Container struct {
	ID        string
	Image     string
	State     string // running, stopped, ...
	IP        string
	Labels    map[string]string
	StartedAt time.Time
}

func (c Container) Running() bool { return c.State == "running" }

type Stat struct {
	MemoryBytes int64
	MemoryLimit int64
	CPUUsec     int64
}

type Image struct {
	Name   string
	Digest string
}

// Runtime is everything Berth needs from a container engine.
type Runtime interface {
	SystemStart(ctx context.Context) error
	SystemStop(ctx context.Context) error
	SystemRunning(ctx context.Context) bool
	Version(ctx context.Context) (string, error)
	EnsureNetwork(ctx context.Context, name string) (gateway string, err error)

	Build(ctx context.Context, o BuildOpts) error
	Run(ctx context.Context, o RunOpts) error
	// RunWith runs a container in the foreground with the given stdio.
	RunWith(ctx context.Context, o RunOpts, stdin io.Reader, stdout io.Writer) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	Remove(ctx context.Context, name string) error
	List(ctx context.Context) ([]Container, error)
	Logs(ctx context.Context, name string, follow bool, tail int, w io.Writer) error
	Exec(ctx context.Context, name string, args []string, interactive, tty bool, stdin io.Reader, stdout, stderr io.Writer) error
	Stats(ctx context.Context) (map[string]Stat, error)

	Images(ctx context.Context) ([]Image, error)
	RemoveImage(ctx context.Context, refs ...string) error
	VolumeCreate(ctx context.Context, name string) error
	VolumeRemove(ctx context.Context, name string) error
	VolumeExists(ctx context.Context, name string) bool
	Volumes(ctx context.Context) ([]string, error)
}

// Find returns the container with the given id from a list.
func Find(cs []Container, id string) (Container, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return Container{}, false
}
