package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Apple drives the `container` CLI.
type Apple struct {
	Bin string
}

func NewApple() *Apple {
	bin := os.Getenv("BERTH_CONTAINER_BIN")
	if bin == "" {
		bin = LookBin("container")
	}
	return &Apple{Bin: bin}
}

// LookBin finds a binary on PATH, falling back to Homebrew's prefix, since
// launchd agents run with a minimal PATH.
func LookBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		p := dir + "/" + name
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func (a *Apple) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, a.Bin, args...)
}

// run executes and returns stdout; stderr is folded into the error.
func (a *Apple) run(ctx context.Context, args ...string) ([]byte, error) {
	start := time.Now()
	c := a.cmd(ctx, args...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	slog.Debug("exec", "cmd", "container "+redactArgs(args), "dur", time.Since(start).Round(time.Millisecond), "err", err)
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.Bytes(), fmt.Errorf("container %s: %v: %s", args[0], err, lastLine(msg))
	}
	return out.Bytes(), nil
}

// redactArgs hides -e values in debug logs; env files are used for secrets
// but multi-line values travel as -e.
func redactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if i > 0 && (args[i-1] == "-e" || args[i-1] == "--env") {
			k, _, _ := strings.Cut(a, "=")
			a = k + "=…"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return s
}

func (a *Apple) SystemStart(ctx context.Context) error {
	_, err := a.run(ctx, "system", "start", "--enable-kernel-install")
	return err
}

func (a *Apple) SystemRunning(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.run(ctx, "system", "status")
	return err == nil
}

func (a *Apple) Version(ctx context.Context) (string, error) {
	out, err := a.run(ctx, "--version")
	if err != nil {
		return "", err
	}
	// "container CLI version 1.6.1 (build: ...)"
	f := strings.Fields(string(out))
	for i, w := range f {
		if w == "version" && i+1 < len(f) {
			return f[i+1], nil
		}
	}
	return strings.TrimSpace(string(out)), nil
}

type networkJSON struct {
	ID     string `json:"id"`
	Status struct {
		Gateway string `json:"ipv4Gateway"`
	} `json:"status"`
}

func (a *Apple) EnsureNetwork(ctx context.Context, name string) (string, error) {
	find := func() (string, bool, error) {
		out, err := a.run(ctx, "network", "list", "--format", "json")
		if err != nil {
			return "", false, err
		}
		var nets []networkJSON
		if err := json.Unmarshal(out, &nets); err != nil {
			return "", false, err
		}
		for _, n := range nets {
			if n.ID == name {
				return n.Status.Gateway, true, nil
			}
		}
		return "", false, nil
	}
	gw, ok, err := find()
	if err != nil {
		return "", err
	}
	if ok {
		return gw, nil
	}
	if _, err := a.run(ctx, "network", "create", "--label", "dev.berth=1", name); err != nil {
		return "", err
	}
	gw, _, err = find()
	return gw, err
}

func (a *Apple) Build(ctx context.Context, o BuildOpts) error {
	args := []string{"build", "--progress", "plain", "-t", o.Tag}
	if o.Dockerfile != "" {
		args = append(args, "-f", o.Dockerfile)
	}
	if o.NoCache {
		args = append(args, "--no-cache")
	}
	for _, ba := range o.BuildArgs {
		args = append(args, "--build-arg", ba)
	}
	args = append(args, o.Context)
	start := time.Now()
	c := a.cmd(ctx, args...)
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	tail := &tailBuffer{max: 4096}
	c.Stdout = io.MultiWriter(out, tail)
	c.Stderr = io.MultiWriter(out, tail)
	err := c.Run()
	slog.Debug("exec", "cmd", "container build "+o.Tag, "dur", time.Since(start).Round(time.Millisecond), "err", err)
	if err != nil {
		return fmt.Errorf("build failed: %s", lastLine(tail.String()))
	}
	return nil
}

func (a *Apple) runArgs(o RunOpts) []string {
	args := []string{"run"}
	if o.Detach {
		args = append(args, "-d")
	}
	if o.Remove {
		args = append(args, "--rm")
	}
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	if o.Network != "" {
		args = append(args, "--network", o.Network)
	}
	for _, p := range o.Publish {
		args = append(args, "-p", p)
	}
	if o.EnvFile != "" {
		args = append(args, "--env-file", o.EnvFile)
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	if o.CPUs != "" {
		args = append(args, "--cpus", o.CPUs)
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	for _, v := range o.Volumes {
		args = append(args, "-v", v)
	}
	keys := make([]string, 0, len(o.Labels))
	for k := range o.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-l", k+"="+o.Labels[k])
	}
	if o.Init {
		args = append(args, "--init")
	}
	if o.Entrypoint != "" {
		args = append(args, "--entrypoint", o.Entrypoint)
	}
	args = append(args, "--progress", "none", o.Image)
	return append(args, o.Args...)
}

// RunArgs exposes the argv for interactive commands the CLI execs directly.
func (a *Apple) RunArgs(o RunOpts) []string { return a.runArgs(o) }

func (a *Apple) Run(ctx context.Context, o RunOpts) error {
	_, err := a.run(ctx, a.runArgs(o)...)
	return err
}

func (a *Apple) RunWith(ctx context.Context, o RunOpts, stdin io.Reader, stdout io.Writer) error {
	c := a.cmd(ctx, a.runArgs(o)...)
	var errb tailBuffer
	errb.max = 2048
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("container run %s: %v: %s", o.Image, err, lastLine(errb.String()))
	}
	return nil
}

func (a *Apple) Start(ctx context.Context, name string) error {
	_, err := a.run(ctx, "start", name)
	return err
}

func (a *Apple) Stop(ctx context.Context, name string, timeout time.Duration) error {
	_, err := a.run(ctx, "stop", "--time", fmt.Sprint(int(timeout.Seconds())), name)
	return err
}

func (a *Apple) Remove(ctx context.Context, name string) error {
	_, err := a.run(ctx, "delete", "--force", name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}

type containerJSON struct {
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
		Image  struct {
			Reference string `json:"reference"`
		} `json:"image"`
	} `json:"configuration"`
	ID     string `json:"id"`
	Status struct {
		State       string `json:"state"`
		StartedDate string `json:"startedDate"`
		Networks    []struct {
			IPv4 string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
	// older CLI versions put status as a plain string
}

func (a *Apple) List(ctx context.Context) ([]Container, error) {
	out, err := a.run(ctx, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var raw []containerJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse container list: %w", err)
	}
	cs := make([]Container, 0, len(raw))
	for _, r := range raw {
		c := Container{ID: r.ID, Image: NormalizeImage(r.Configuration.Image.Reference), State: r.Status.State, Labels: r.Configuration.Labels}
		if c.ID == "" {
			c.ID = r.Configuration.ID
		}
		if len(r.Status.Networks) > 0 {
			c.IP = strings.Split(r.Status.Networks[0].IPv4, "/")[0]
		}
		if t, err := time.Parse(time.RFC3339, r.Status.StartedDate); err == nil {
			c.StartedAt = t
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// NormalizeImage strips the docker.io/ and library/ prefixes `container`
// adds, so berth/app:3 compares equal to docker.io/berth/app:3.
func NormalizeImage(ref string) string {
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

func (a *Apple) Logs(ctx context.Context, name string, follow bool, tail int, w io.Writer) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	if tail >= 0 {
		args = append(args, "-n", fmt.Sprint(tail))
	}
	args = append(args, name)
	c := a.cmd(ctx, args...)
	c.Stdout, c.Stderr = w, w
	return c.Run()
}

func (a *Apple) Exec(ctx context.Context, name string, args []string, interactive, tty bool, stdin io.Reader, stdout, stderr io.Writer) error {
	full := []string{"exec"}
	if interactive {
		full = append(full, "-i")
	}
	if tty {
		full = append(full, "-t")
	}
	full = append(full, name)
	full = append(full, args...)
	start := time.Now()
	c := a.cmd(ctx, full...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	var errb tailBuffer
	errb.max = 2048
	if stderr == nil {
		c.Stderr = &errb
	} else {
		c.Stderr = io.MultiWriter(stderr, &errb)
	}
	err := c.Run()
	slog.Debug("exec", "cmd", "container exec "+name+" "+args[0], "dur", time.Since(start).Round(time.Millisecond), "err", err)
	if err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%v: %s", err, lastLine(msg))
		}
	}
	return err
}

func (a *Apple) Stats(ctx context.Context) (map[string]Stat, error) {
	out, err := a.run(ctx, "stats", "--no-stream", "--format", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID    string `json:"id"`
		Mem   int64  `json:"memoryUsageBytes"`
		Limit int64  `json:"memoryLimitBytes"`
		CPU   int64  `json:"cpuUsageUsec"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	m := map[string]Stat{}
	for _, r := range raw {
		m[r.ID] = Stat{MemoryBytes: r.Mem, MemoryLimit: r.Limit, CPUUsec: r.CPU}
	}
	return m, nil
}

func (a *Apple) Images(ctx context.Context) ([]Image, error) {
	out, err := a.run(ctx, "image", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID            string `json:"id"`
		Configuration struct {
			Name string `json:"name"`
		} `json:"configuration"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var imgs []Image
	for _, r := range raw {
		name := r.Configuration.Name
		if name == "" {
			name = r.Name
		}
		imgs = append(imgs, Image{Name: NormalizeImage(name), Digest: r.ID})
	}
	return imgs, nil
}

func (a *Apple) RemoveImage(ctx context.Context, refs ...string) error {
	if len(refs) == 0 {
		return nil
	}
	_, err := a.run(ctx, append([]string{"image", "delete"}, refs...)...)
	return err
}

func (a *Apple) VolumeCreate(ctx context.Context, name string) error {
	if a.VolumeExists(ctx, name) {
		return nil
	}
	_, err := a.run(ctx, "volume", "create", "--label", "dev.berth=1", name)
	return err
}

func (a *Apple) VolumeRemove(ctx context.Context, name string) error {
	_, err := a.run(ctx, "volume", "delete", name)
	return err
}

func (a *Apple) VolumeExists(ctx context.Context, name string) bool {
	_, err := a.run(ctx, "volume", "inspect", name)
	return err == nil
}

func (a *Apple) Volumes(ctx context.Context) ([]string, error) {
	out, err := a.run(ctx, "volume", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID            string `json:"id"`
		Configuration struct {
			Name string `json:"name"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var names []string
	for _, r := range raw {
		n := r.Configuration.Name
		if n == "" {
			n = r.ID
		}
		names = append(names, n)
	}
	return names, nil
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
