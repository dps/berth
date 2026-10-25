package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LocalModel is a model Ollama has downloaded.
type LocalModel struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// LoadedModel is a model Ollama holds in memory.
type LoadedModel struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ollamaGet(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// OllamaVersion reports Ollama's version, or an error if it isn't running.
func OllamaVersion(ctx context.Context, base string) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := ollamaGet(ctx, base+"/api/version", &v)
	return v.Version, err
}

// OllamaModels lists downloaded models.
func OllamaModels(ctx context.Context, base string) ([]LocalModel, error) {
	var v struct {
		Models []LocalModel `json:"models"`
	}
	err := ollamaGet(ctx, base+"/api/tags", &v)
	return v.Models, err
}

// OllamaLoaded lists models in memory.
func OllamaLoaded(ctx context.Context, base string) ([]LoadedModel, error) {
	var v struct {
		Models []LoadedModel `json:"models"`
	}
	err := ollamaGet(ctx, base+"/api/ps", &v)
	return v.Models, err
}

// PullProgress is one line of Ollama's pull stream.
type PullProgress struct {
	Status    string `json:"status"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

// OllamaPull downloads a model, reporting progress.
func OllamaPull(ctx context.Context, base, model string, progress func(PullProgress)) error {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pull %s: %s %s", model, resp.Status, bytes.TrimSpace(b))
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var p PullProgress
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		if p.Error != "" {
			return fmt.Errorf("pull %s: %s", model, p.Error)
		}
		if progress != nil {
			progress(p)
		}
	}
	return sc.Err()
}

// RecommendedModel picks the default local model for a Mac with mem bytes
// of RAM, leaving room for apps and databases: qwen3:8b (about 5 GB) from
// 16 GB, qwen3:4b (about 2.5 GB) below that.
func RecommendedModel(mem uint64) string {
	if mem >= 15<<30 {
		return "qwen3:8b"
	}
	return "qwen3:4b"
}
