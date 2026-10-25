package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir string, files map[string]string) {
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		stack string
		want  []string
		not   []string
	}{
		{"node npm start", map[string]string{"package.json": `{"scripts":{"start":"node s.js","build":"tsc"},"engines":{"node":">=20.5"}}`, "package-lock.json": "{}"},
			"node", []string{"FROM node:20-slim", "--mount=type=cache,target=/root/.npm", "npm ci", "RUN npm run build", `CMD ["npm","start"]`}, nil},
		{"node no start", map[string]string{"package.json": `{}`, "server.js": ""},
			"node", []string{"FROM node:22-slim", "npm install", `CMD ["node","server.js"]`}, []string{"npm run build"}},
		{"pnpm", map[string]string{"package.json": `{"scripts":{"start":"x"}}`, "pnpm-lock.yaml": ""},
			"node", []string{"corepack enable", "pnpm install --frozen-lockfile", `CMD ["pnpm","start"]`}, nil},
		{"bun beats node", map[string]string{"package.json": `{}`, "bun.lock": "", "index.ts": ""},
			"bun", []string{"FROM oven/bun:1", `CMD ["bun","run","index.ts"]`}, nil},
		{"fastapi", map[string]string{"requirements.txt": "fastapi\nuvicorn[standard]\n", "main.py": "from fastapi import FastAPI\napp = FastAPI()\n"},
			"python", []string{"COPY requirements.txt ./", "--mount=type=cache,target=/root/.cache/uv uv pip install --system -r requirements.txt", "uvicorn main:app --host 0.0.0.0 --port ${PORT}"}, nil},
		{"flask gunicorn", map[string]string{"requirements.txt": "flask\ngunicorn\n", "app.py": "from flask import Flask\nserver = Flask(__name__)\n"},
			"python", []string{"gunicorn --bind 0.0.0.0:${PORT} app:server"}, nil},
		{"uv project", map[string]string{"pyproject.toml": "[project]\nrequires-python = \">=3.13\"\n", "uv.lock": "", "main.py": ""},
			"python", []string{"FROM python:3.13-slim", "COPY pyproject.toml uv.lock ./", "--no-install-project", "uv sync --frozen --no-dev\n", "/app/.venv/bin", `CMD ["python","main.py"]`}, nil},
		{"go cmd dir", map[string]string{"go.mod": "module x\n\ngo 1.23.4\n", "cmd/server/main.go": "package main"},
			"go", []string{"FROM golang:1.23 AS build", "--mount=type=cache,target=/root/.cache/go-build", "./cmd/server"}, nil},
		{"rust", map[string]string{"Cargo.toml": "[package]\nname = \"hello-web\"\nversion = \"0.1.0\"\n"},
			"rust", []string{"cp target/release/hello-web /usr/local/bin/app", "id=berth-cargo-target-hello-web"}, nil},
		{"static", map[string]string{"index.html": "<h1>hi</h1>"},
			"static", []string{"caddy file-server"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, c.files)
			res, err := Detect(dir)
			if err != nil {
				t.Fatal(err)
			}
			if res.Stack != c.stack {
				t.Fatalf("stack %s, want %s", res.Stack, c.stack)
			}
			for _, w := range c.want {
				if !strings.Contains(res.Dockerfile, w) {
					t.Errorf("missing %q in:\n%s", w, res.Dockerfile)
				}
			}
			for _, n := range c.not {
				if strings.Contains(res.Dockerfile, n) {
					t.Errorf("unexpected %q", n)
				}
			}
		})
	}
}

func TestDetectNothing(t *testing.T) {
	if _, err := Detect(t.TempDir()); err == nil || !strings.Contains(err.Error(), "add a Dockerfile") {
		t.Fatalf("got %v", err)
	}
}
