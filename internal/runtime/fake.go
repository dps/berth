package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Runtime for tests. Run starts a real HTTP server on
// the published host port so health checks and proxying work end to end.
// Images whose Dockerfile context contains "FAIL_HEALTH" serve 503;
// "FAIL_BUILD" fails the build.
type Fake struct {
	mu         sync.Mutex
	Containers map[string]*fakeContainer
	ImageSet   map[string]string // image -> behaviour ("ok", "unhealthy")
	VolumeSet  map[string]bool
	Calls      []string
	BuildHook  func(o BuildOpts) (behaviour string, err error)
	RunWithErr error // returned by RunWith (e.g. a failing network probe)
}

type fakeContainer struct {
	opts    RunOpts
	state   string
	srv     *http.Server
	started time.Time
}

func NewFake() *Fake {
	return &Fake{Containers: map[string]*fakeContainer{}, ImageSet: map[string]string{}, VolumeSet: map[string]bool{}}
}

func (f *Fake) record(s string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, s)
	f.mu.Unlock()
}

// CallLog returns calls matching a prefix, in order.
func (f *Fake) CallLog(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) SystemStart(ctx context.Context) error { return nil }
func (f *Fake) SystemStop(ctx context.Context) error {
	f.record("system stop")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Containers {
		c.stop()
	}
	return nil
}

func (f *Fake) SystemRunning(ctx context.Context) bool      { return true }
func (f *Fake) Version(ctx context.Context) (string, error) { return "fake", nil }
func (f *Fake) EnsureNetwork(ctx context.Context, name string) (string, error) {
	return "127.0.0.1", nil
}

func (f *Fake) Build(ctx context.Context, o BuildOpts) error {
	f.record("build " + o.Tag)
	behaviour := "ok"
	if f.BuildHook != nil {
		b, err := f.BuildHook(o)
		if err != nil {
			return err
		}
		if b != "" {
			behaviour = b
		}
	}
	if o.Out != nil {
		fmt.Fprintf(o.Out, "#1 fake build %s\n", o.Tag)
	}
	f.mu.Lock()
	f.ImageSet[o.Tag] = behaviour
	f.mu.Unlock()
	return nil
}

func (f *Fake) Run(ctx context.Context, o RunOpts) error {
	f.record("run " + o.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Containers[o.Name]; ok {
		return fmt.Errorf("container %s already exists", o.Name)
	}
	behaviour, ok := f.ImageSet[o.Image]
	if !ok {
		behaviour = "ok"
	}
	c := &fakeContainer{opts: o, state: "running", started: time.Now()}
	if err := c.serve(behaviour); err != nil {
		return err
	}
	f.Containers[o.Name] = c
	return nil
}

func (c *fakeContainer) serve(behaviour string) error {
	for _, p := range c.opts.Publish {
		parts := strings.Split(p, ":")
		if len(parts) < 2 {
			continue
		}
		addr := "127.0.0.1:" + parts[len(parts)-2]
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		name := c.opts.Name
		c.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if behaviour == "unhealthy" {
				w.WriteHeader(503)
				return
			}
			fmt.Fprintf(w, "hello from %s\n", name)
		})}
		go c.srv.Serve(ln)
		break
	}
	return nil
}

func (f *Fake) RunWith(ctx context.Context, o RunOpts, stdin io.Reader, stdout io.Writer) error {
	f.record("runwith " + o.Image + " " + strings.Join(o.Args, " "))
	if f.RunWithErr != nil {
		return f.RunWithErr
	}
	if stdin != nil {
		io.Copy(io.Discard, stdin)
	}
	return nil
}

func (f *Fake) Start(ctx context.Context, name string) error {
	f.record("start " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[name]
	if !ok {
		return fmt.Errorf("container %s not found", name)
	}
	if c.state != "running" {
		c.state = "running"
		c.started = time.Now()
		return c.serve(f.ImageSet[c.opts.Image])
	}
	return nil
}

func (f *Fake) Stop(ctx context.Context, name string, timeout time.Duration) error {
	f.record("stop " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.Containers[name]; ok {
		c.stop()
	}
	return nil
}

func (c *fakeContainer) stop() {
	if c.srv != nil {
		c.srv.Close()
		c.srv = nil
	}
	c.state = "stopped"
}

// Crash simulates the container's process exiting.
func (f *Fake) Crash(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.Containers[name]; ok {
		c.stop()
	}
}

func (f *Fake) Remove(ctx context.Context, name string) error {
	f.record("rm " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.Containers[name]; ok {
		c.stop()
		delete(f.Containers, name)
	}
	return nil
}

func (f *Fake) List(ctx context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var cs []Container
	for name, c := range f.Containers {
		cs = append(cs, Container{ID: name, Image: c.opts.Image, State: c.state, Labels: c.opts.Labels, StartedAt: c.started})
	}
	return cs, nil
}

func (f *Fake) Logs(ctx context.Context, name string, follow bool, tail int, w io.Writer) error {
	fmt.Fprintf(w, "log line from %s\n", name)
	if follow {
		<-ctx.Done()
	}
	return nil
}

func (f *Fake) Exec(ctx context.Context, name string, args []string, interactive, tty bool, stdin io.Reader, stdout, stderr io.Writer) error {
	f.record("exec " + name + " " + strings.Join(args, " "))
	return nil
}

func (f *Fake) Stats(ctx context.Context) (map[string]Stat, error) { return map[string]Stat{}, nil }

func (f *Fake) Images(ctx context.Context) ([]Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Image
	for name := range f.ImageSet {
		out = append(out, Image{Name: name})
	}
	return out, nil
}

func (f *Fake) RemoveImage(ctx context.Context, refs ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range refs {
		f.Calls = append(f.Calls, "rmi "+r)
		delete(f.ImageSet, r)
	}
	return nil
}

func (f *Fake) VolumeCreate(ctx context.Context, name string) error {
	f.mu.Lock()
	f.VolumeSet[name] = true
	f.mu.Unlock()
	return nil
}

func (f *Fake) VolumeRemove(ctx context.Context, name string) error {
	f.mu.Lock()
	delete(f.VolumeSet, name)
	f.mu.Unlock()
	return nil
}

func (f *Fake) Volumes(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for v := range f.VolumeSet {
		out = append(out, v)
	}
	return out, nil
}

func (f *Fake) VolumeExists(ctx context.Context, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.VolumeSet[name]
}
