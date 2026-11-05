package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dps/berth/internal/ai"
	"github.com/dps/berth/internal/runtime"
	"github.com/dps/berth/internal/state"
)

// ---- MLX worker (design.md, AI gateway) ----

// The worker is a Python process, since MLX is a Python library: a venv
// under ai/mlx with mlx-lm, the model in ai/mlx/hf, and the script berthd
// carries. It runs while ai/config.json has an "mlx" section.

func (d *Daemon) mlxDir() string { return d.Store.Path("ai", "mlx") }

func (d *Daemon) mlxPython() string { return filepath.Join(d.mlxDir(), "venv", "bin", "python") }

// mlxChild runs the worker, restarting it if it dies.
type mlxChild struct {
	d    *Daemon
	want chan bool

	mu   sync.Mutex
	proc *os.Process
}

func (c *mlxChild) setProc(p *os.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proc = p
}

func (c *mlxChild) set(on bool) {
	select {
	case <-c.want:
	default:
	}
	c.want <- on
}

// restart stops a running worker, which the supervisor starts again with
// the current setup.
func (c *mlxChild) restart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proc != nil {
		c.proc.Signal(syscall.SIGTERM)
	}
}

func (d *Daemon) startMLX(ctx context.Context) {
	d.mlx = &mlxChild{d: d, want: make(chan bool, 1)}
	go superviseChild(ctx, "MLX worker", d.mlx.want, d.mlx.setProc, d.mlxCommand)
	d.configureMLX()
}

// configureMLX runs the worker when it's set up.
func (d *Daemon) configureMLX() {
	if d.mlx == nil {
		return
	}
	ac, err := d.Store.AIConfig()
	_, venv := os.Stat(d.mlxPython())
	d.mlx.set(err == nil && ac.MLX != nil && venv == nil)
}

func (d *Daemon) mlxCommand(ctx context.Context) (*exec.Cmd, error) {
	ac, err := d.Store.AIConfig()
	if err != nil || ac.MLX == nil {
		return nil, fmt.Errorf("the MLX worker is off")
	}
	script := ac.MLX.Worker
	if script == "" {
		// Written each start, so a new berthd brings its worker with it.
		script = filepath.Join(d.mlxDir(), "worker.py")
		if err := state.WriteFile(script, ai.MLXWorker, 0o644); err != nil {
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, d.mlxPython(), script)
	cmd.Env = append(os.Environ(), d.mlxEnv(ac.MLX)...)
	cmd.Env = append(cmd.Env, "BERTH_MLX_ADDR="+ai.MLXAddr(ac))
	if ac.MLX.IdleSeconds > 0 {
		cmd.Env = append(cmd.Env, "BERTH_MLX_IDLE="+strconv.Itoa(ac.MLX.IdleSeconds))
	}
	return cmd, nil
}

// mlxPythonVersion is the Python uv installs for the worker: one MLX and
// its dependencies all have wheels for.
const mlxPythonVersion = "3.12"

// saeDir is where a repo's autoencoder layers are kept, converted.
func (d *Daemon) saeDir(repo string) string {
	return filepath.Join(d.mlxDir(), "sae", strings.ReplaceAll(repo, "/", "--"))
}

func (d *Daemon) mlxEnv(m *state.AIMLX) []string {
	env := []string{}
	if m.SAE != "" {
		env = append(env, "BERTH_MLX_SAE="+m.SAE, "BERTH_MLX_SAE_DIR="+d.saeDir(m.SAE))
	}
	return append(env,
		"BERTH_MLX_MODEL="+m.Model,
		"HF_HOME=" + filepath.Join(d.mlxDir(), "hf"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(d.mlxDir(), "python"),
		"UV_CACHE_DIR=" + filepath.Join(d.mlxDir(), "uv-cache"),
		"HF_HUB_DISABLE_TELEMETRY=1",
		"PYTHONUNBUFFERED=1",
	)
}

// superviseChild starts a child process when it's wanted, and again if it
// dies, waiting longer each time it dies quickly. The child must exit when
// its stdin closes, so it can't outlive berthd, even across a crash or a
// re-exec.
func superviseChild(ctx context.Context, name string, want chan bool, setProc func(*os.Process), command func(context.Context) (*exec.Cmd, error)) {
	wanted := false
	delay := time.Second
	for {
		for !wanted {
			select {
			case wanted = <-want:
			case <-ctx.Done():
				return
			}
		}
		cmd, err := command(ctx)
		var stdin io.WriteCloser
		if err == nil {
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			stdin, err = cmd.StdinPipe()
		}
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			slog.Error(name, "err", err)
			select {
			case <-time.After(delay):
			case wanted = <-want:
			case <-ctx.Done():
				return
			}
			delay = min(delay*2, 30*time.Second)
			continue
		}
		setProc(cmd.Process)
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
	running:
		for {
			select {
			case wanted = <-want:
				if !wanted {
					stdin.Close()
				}
			case err := <-done:
				setProc(nil)
				if ctx.Err() != nil {
					return
				}
				if !wanted {
					break running
				}
				slog.Warn(name+" exited; restarting", "err", err)
				if time.Since(started) > time.Minute {
					delay = time.Second
				}
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
				delay = min(delay*2, 30*time.Second)
				break running
			}
		}
		stdin.Close()
	}
}

type AIMLXParams struct {
	Model     string `json:"model,omitempty"`
	Off       bool   `json:"off,omitempty"`
	SAE       string `json:"sae,omitempty"`        // a repo of sparse autoencoders; "none" drops them
	SAELayers []int  `json:"sae_layers,omitempty"` // which of its layers to fetch
}

// hAIMLX sets the worker up (Python environment, model download) and turns
// it on, or off. Run again, it checks the setup and restarts the worker.
func (d *Daemon) hAIMLX(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p AIMLXParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	ac, err := d.Store.AIConfig()
	if err != nil {
		return nil, err
	}
	if p.Off {
		ac.MLX = nil
		if err := d.Store.SaveAIConfig(ac); err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "-----> MLX worker off (the model stays in %s; delete it to free the space)\n", filepath.Join(d.mlxDir(), "hf"))
		d.configureMLX()
		return map[string]bool{"on": false}, d.Apply(ctx)
	}
	m := &state.AIMLX{Model: ai.DefaultMLXModel}
	if ac.MLX != nil {
		m = ac.MLX
	}
	if p.Model != "" && p.Model != m.Model {
		m.Model = p.Model
		m.SAE, m.SAELayers = "", nil // trained for the old model
	}
	switch {
	case p.SAE == "none":
		if m.SAE != "" {
			fmt.Fprintf(out, "-----> Features off (they stay in %s; delete it to free the space)\n", d.saeDir(m.SAE))
		}
		m.SAE, m.SAELayers = "", nil
	case p.SAE != "":
		if p.SAE != m.SAE {
			m.SAELayers = nil
		}
		m.SAE = p.SAE
	}
	if len(p.SAELayers) > 0 {
		if m.SAE == "" {
			return nil, fmt.Errorf("--sae-layers needs --sae, a repo of sparse autoencoders for %s", m.Model)
		}
		m.SAELayers = p.SAELayers
	}
	if err := d.mlxSetup(ctx, m, out); err != nil {
		return nil, err
	}
	ac.MLX = m
	if err := d.Store.SaveAIConfig(ac); err != nil {
		return nil, err
	}
	if err := d.Apply(ctx); err != nil {
		return nil, err
	}
	d.configureMLX()
	d.mlx.restart()
	addr := ai.MLXAddr(ac)
	fmt.Fprintf(out, "-----> Starting the worker on %s\n", addr)
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the MLX worker didn't start; see ~/.berth/logs/berthd.log")
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	fmt.Fprintf(out, "=====> %s/%s is ready: the model loads on first use and unloads when idle\n", ai.MLX, ai.MLXName(ac))
	return map[string]any{"on": true, "model": ai.MLX + "/" + ai.MLXName(ac)}, nil
}

// mlxSetup makes the Python environment and downloads the model, both
// only when missing.
func (d *Daemon) mlxSetup(ctx context.Context, m *state.AIMLX, out io.Writer) error {
	if err := os.MkdirAll(d.mlxDir(), 0o755); err != nil {
		return err
	}
	run := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), d.mlxEnv(m)...)
		cmd.Stdout, cmd.Stderr = out, out
		return cmd.Run()
	}
	// uv makes the environment with a Python of its own, kept under ai/mlx,
	// so the Mac's Python, broken or missing, doesn't matter.
	uv := runtime.LookBin("uv")
	if _, err := exec.LookPath(uv); err != nil {
		return fmt.Errorf("MLX is set up with uv: brew install uv, then berth ai:mlx")
	}
	venv := filepath.Join(d.mlxDir(), "venv")
	vpy := d.mlxPython()
	if _, err := os.Stat(vpy); err != nil {
		fmt.Fprintf(out, "-----> Making a Python %s environment in %s\n", mlxPythonVersion, venv)
		if err := run(uv, "venv", "--quiet", "--python", mlxPythonVersion, venv); err != nil {
			return fmt.Errorf("making the Python environment: %w", err)
		}
	}
	check := fmt.Sprintf("import mlx.core, mlx_lm; assert mlx_lm.__version__ == %q", ai.MLXPackage[len("mlx-lm=="):])
	if exec.CommandContext(ctx, vpy, "-c", check).Run() != nil {
		fmt.Fprintf(out, "-----> Installing %s (MLX runs models on the Mac's GPU)\n", ai.MLXPackage)
		if err := run(uv, "pip", "install", "--quiet", "--python", vpy, ai.MLXPackage); err != nil {
			return fmt.Errorf("installing %s: %w", ai.MLXPackage, err)
		}
	}
	fmt.Fprintf(out, "-----> Getting %s (once; several GB)\n", m.Model)
	get := fmt.Sprintf("from huggingface_hub import snapshot_download; print(snapshot_download(%q, allow_patterns=['*.json', '*.safetensors', '*.txt', '*.jinja', '*.model', '*.tiktoken']))", m.Model)
	if err := run(vpy, "-c", get); err != nil {
		return fmt.Errorf("downloading %s: %w", m.Model, err)
	}
	if m.SAE == "" {
		return nil
	}
	// The worker script fetches and converts the autoencoder's layers;
	// each is about 2 GB to download and 1 GB kept.
	script := filepath.Join(d.mlxDir(), "worker.py")
	if m.Worker != "" {
		script = m.Worker
	} else if err := state.WriteFile(script, ai.MLXWorker, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "-----> Getting %s's features\n", m.SAE)
	args := []string{script, "--get-sae", m.SAE, d.saeDir(m.SAE)}
	for _, l := range m.SAELayers {
		args = append(args, strconv.Itoa(l))
	}
	if err := run(vpy, args...); err != nil {
		return fmt.Errorf("getting %s: %w", m.SAE, err)
	}
	return nil
}
