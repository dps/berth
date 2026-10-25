// Package detect picks a Dockerfile for repos that do not ship one. The
// order matters: bun.lock(b) → Bun; package.json → Node; pyproject.toml /
// requirements.txt → Python; go.mod → Go; Cargo.toml → Rust; index.html with
// no manifest → static.
package detect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/dps/berth/templates"
)

type Result struct {
	Stack      string // bun, node, python, go, rust, static
	Dockerfile string // rendered template
}

// Detect inspects dir and renders a Dockerfile, or returns an error saying
// what it looked for.
func Detect(dir string) (*Result, error) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	var stack string
	var data map[string]any
	var err error
	switch {
	case has("bun.lockb") || has("bun.lock"):
		stack = "bun"
		data, err = bunData(dir)
	case has("package.json"):
		stack = "node"
		data, err = nodeData(dir, has)
	case has("pyproject.toml") || has("requirements.txt"):
		stack = "python"
		data, err = pythonData(dir, has)
	case has("go.mod"):
		stack = "go"
		data, err = goData(dir, has)
	case has("Cargo.toml"):
		stack = "rust"
		data, err = rustData(dir)
	case has("index.html"):
		stack = "static"
		data = map[string]any{}
	default:
		return nil, fmt.Errorf("no Dockerfile and no recognised stack (looked for package.json, bun.lock, pyproject.toml, requirements.txt, go.mod, Cargo.toml, index.html); add a Dockerfile")
	}
	if err != nil {
		return nil, err
	}
	out, err := Render(stack, data)
	if err != nil {
		return nil, err
	}
	return &Result{Stack: stack, Dockerfile: out}, nil
}

func Render(stack string, data map[string]any) (string, error) {
	src, err := templates.FS.ReadFile(stack + ".Dockerfile")
	if err != nil {
		return "", err
	}
	t, err := template.New(stack).Parse(string(src))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// execForm renders a CMD in exec form, wrapped in sh -c when it needs
// variable expansion.
func execForm(cmd string) string {
	if strings.Contains(cmd, "$") {
		b, _ := json.Marshal([]string{"sh", "-c", "exec " + cmd})
		return string(b)
	}
	b, _ := json.Marshal(strings.Fields(cmd))
	return string(b)
}

type packageJSON struct {
	Main    string            `json:"main"`
	Scripts map[string]string `json:"scripts"`
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
	PackageManager string `json:"packageManager"`
}

func readPackageJSON(dir string) (*packageJSON, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	p := &packageJSON{}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	return p, nil
}

var majorRe = regexp.MustCompile(`(\d+)`)

// nodeMajor turns an engines.node range like ">=20", "^22.1", "20.x" into a
// major version tag, defaulting to the current LTS.
func nodeMajor(r string) string {
	if m := majorRe.FindString(r); m != "" {
		return m
	}
	return "22"
}

func nodeStart(dir string, p *packageJSON, runner string) string {
	if _, ok := p.Scripts["start"]; ok {
		return execForm(runner + " start")
	}
	for _, f := range []string{p.Main, "server.js", "index.js", "app.js", "main.js"} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return execForm("node " + f)
		}
	}
	return execForm(runner + " start")
}

func nodeData(dir string, has func(string) bool) (map[string]any, error) {
	p, err := readPackageJSON(dir)
	if err != nil {
		return nil, err
	}
	d := map[string]any{"NodeVersion": nodeMajor(p.Engines.Node)}
	switch {
	case has("pnpm-lock.yaml"):
		d["Corepack"] = true
		d["LockFiles"] = "pnpm-lock.yaml"
		d["InstallCmd"] = "pnpm install --frozen-lockfile"
		if _, ok := p.Scripts["build"]; ok {
			d["BuildCmd"] = "pnpm run build"
		}
		d["StartCmd"] = nodeStart(dir, p, "pnpm")
	case has("yarn.lock"):
		d["Corepack"] = true
		d["LockFiles"] = "yarn.lock"
		d["InstallCmd"] = "yarn install --frozen-lockfile || yarn install"
		if _, ok := p.Scripts["build"]; ok {
			d["BuildCmd"] = "yarn build"
		}
		d["StartCmd"] = nodeStart(dir, p, "yarn")
	default:
		if has("package-lock.json") {
			d["LockFiles"] = "package-lock.json"
			d["InstallCmd"] = "npm ci"
		} else {
			d["LockFiles"] = ""
			d["InstallCmd"] = "npm install"
		}
		if _, ok := p.Scripts["build"]; ok {
			d["BuildCmd"] = "npm run build"
		}
		d["StartCmd"] = nodeStart(dir, p, "npm")
	}
	return d, nil
}

func bunData(dir string) (map[string]any, error) {
	p, err := readPackageJSON(dir)
	if err != nil {
		return nil, err
	}
	var locks []string
	for _, f := range []string{"bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			locks = append(locks, f)
		}
	}
	d := map[string]any{"LockFiles": strings.Join(locks, " ")}
	if _, ok := p.Scripts["build"]; ok {
		d["BuildCmd"] = "bun run build"
	}
	if _, ok := p.Scripts["start"]; ok {
		d["StartCmd"] = execForm("bun run start")
	} else {
		entry := "index.ts"
		for _, f := range []string{p.Main, "index.ts", "server.ts", "src/index.ts", "index.js", "server.js"} {
			if f == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				entry = f
				break
			}
		}
		d["StartCmd"] = execForm("bun run " + entry)
	}
	return d, nil
}

func pythonData(dir string, has func(string) bool) (map[string]any, error) {
	d := map[string]any{"PythonVersion": "3.12"}
	var deps string
	if b, err := os.ReadFile(filepath.Join(dir, "requirements.txt")); err == nil {
		deps += strings.ToLower(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil {
		s := string(b)
		deps += strings.ToLower(s)
		if m := regexp.MustCompile(`requires-python\s*=\s*"[^"]*?(3\.\d+)`).FindStringSubmatch(s); m != nil {
			d["PythonVersion"] = m[1]
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".python-version")); err == nil {
		if m := regexp.MustCompile(`3\.\d+`).FindString(string(b)); m != "" {
			d["PythonVersion"] = m
		}
	}
	switch {
	case has("uv.lock"):
		d["InstallCmd"] = "uv sync --frozen --no-dev"
		d["VenvPath"] = true
	case has("requirements.txt"):
		d["InstallCmd"] = "uv pip install --system -r requirements.txt"
	default:
		d["InstallCmd"] = "uv pip install --system -r pyproject.toml"
	}
	d["StartCmd"] = pythonStart(dir, deps, has)
	return d, nil
}

var appVarRe = regexp.MustCompile(`(?m)^(\w+)\s*=\s*(FastAPI|Flask|Starlette|Quart|Sanic|Litestar)\(`)

func pythonStart(dir, deps string, has func(string) bool) string {
	// Find module:app for ASGI/WSGI frameworks.
	module, appVar, framework := "", "", ""
	for _, f := range []string{"main.py", "app.py", "server.py", "api.py", "app/main.py", "src/main.py"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if m := appVarRe.FindStringSubmatch(string(b)); m != nil {
			module = strings.ReplaceAll(strings.TrimSuffix(f, ".py"), "/", ".")
			appVar, framework = m[1], m[2]
			break
		}
	}
	if has("manage.py") {
		if wsgi := findDjangoWSGI(dir); wsgi != "" {
			return execForm("gunicorn --bind 0.0.0.0:${PORT} " + wsgi + ":application")
		}
	}
	if module != "" {
		target := module + ":" + appVar
		asgi := framework != "Flask"
		switch {
		case asgi && strings.Contains(deps, "uvicorn"):
			return execForm("uvicorn " + target + " --host 0.0.0.0 --port ${PORT}")
		case strings.Contains(deps, "gunicorn"):
			if asgi && strings.Contains(deps, "uvicorn") {
				return execForm("gunicorn -k uvicorn.workers.UvicornWorker --bind 0.0.0.0:${PORT} " + target)
			}
			return execForm("gunicorn --bind 0.0.0.0:${PORT} " + target)
		case framework == "Flask":
			return execForm("flask --app " + target + " run --host 0.0.0.0 --port ${PORT}")
		}
	}
	for _, f := range []string{"main.py", "app.py", "server.py"} {
		if has(f) {
			return execForm("python " + f)
		}
	}
	return execForm("python main.py")
}

func findDjangoWSGI(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "wsgi.py"))
	if len(matches) == 0 {
		return ""
	}
	return filepath.Base(filepath.Dir(matches[0])) + ".wsgi"
}

var goVersionRe = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)

func goData(dir string, has func(string) bool) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, err
	}
	d := map[string]any{"GoVersion": "1", "MainPkg": "."}
	if m := goVersionRe.FindSubmatch(b); m != nil {
		d["GoVersion"] = string(m[1])
	}
	if !has("main.go") {
		if cmds, _ := filepath.Glob(filepath.Join(dir, "cmd", "*", "main.go")); len(cmds) > 0 {
			d["MainPkg"] = "./cmd/" + filepath.Base(filepath.Dir(cmds[0]))
		}
	}
	return d, nil
}

var cargoNameRe = regexp.MustCompile(`(?s)\[package\].*?\bname\s*=\s*"([^"]+)"`)
var cargoBinRe = regexp.MustCompile(`(?s)\[\[bin\]\].*?\bname\s*=\s*"([^"]+)"`)

func rustData(dir string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return nil, err
	}
	name := ""
	if m := cargoBinRe.FindSubmatch(b); m != nil {
		name = string(m[1])
	} else if m := cargoNameRe.FindSubmatch(b); m != nil {
		name = string(m[1])
	}
	if name == "" {
		return nil, fmt.Errorf("Cargo.toml: no [package] name")
	}
	return map[string]any{"Binary": name}, nil
}
