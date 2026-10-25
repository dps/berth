package state

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// AIConfig is the AI gateway's shared setup (design.md, AI gateway), kept
// in ai/config.json. Provider keys live in secrets/ai.json.
type AIConfig struct {
	// Aliases map short names apps ask for ("default", "smart") to
	// provider/model.
	Aliases map[string]string `json:"aliases,omitempty"`
	// Providers adds OpenAI-compatible endpoints beyond the built-in ones.
	Providers map[string]AIProvider `json:"providers,omitempty"`
	// OllamaURL is where local models are served (default
	// http://127.0.0.1:11434).
	OllamaURL string `json:"ollama_url,omitempty"`
	// LocalParallel caps concurrent requests to local models; the rest
	// queue (default 2).
	LocalParallel int `json:"local_parallel,omitempty"`
}

type AIProvider struct {
	URL string `json:"url"` // OpenAI-compatible base, ending in /v1 or similar
}

// AppAI is an app's access to the gateway.
type AppAI struct {
	TokenHash string `json:"token_hash"` // sha256 hex of the app's berth_ai_ token
	// Models the app may use: alias names, provider/model, or provider/*.
	// Empty means any.
	Models []string `json:"models,omitempty"`
	// MonthlyTokens caps input+output tokens per calendar month; 0 = no cap.
	MonthlyTokens int64 `json:"monthly_tokens,omitempty"`
	// RPM caps requests per minute; 0 = no cap.
	RPM int `json:"rpm,omitempty"`
}

func (s *Store) AIConfig() (*AIConfig, error) {
	c := &AIConfig{}
	if err := readJSON(s.Path("ai", "config.json"), c); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if c.Aliases == nil {
		c.Aliases = map[string]string{}
	}
	if c.Providers == nil {
		c.Providers = map[string]AIProvider{}
	}
	return c, nil
}

func (s *Store) SaveAIConfig(c *AIConfig) error {
	if err := os.MkdirAll(s.Path("ai"), 0o700); err != nil {
		return err
	}
	return writeJSON(s.Path("ai", "config.json"), c, 0o600)
}

// AIConfigured reports whether the gateway has been set up at all.
func (s *Store) AIConfigured() bool {
	_, err := os.Stat(s.Path("ai", "config.json"))
	return err == nil
}

var aiName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateAIName checks an alias or provider name.
func ValidateAIName(kind, name string) error {
	if !aiName.MatchString(name) {
		return fmt.Errorf("%s %q: use lowercase letters, digits, '.', '_' and '-'", kind, name)
	}
	return nil
}

// ValidateAIModel checks a provider/model reference (model names may
// contain '/', ':' and '.', as in openrouter/meta-llama/llama-3 or
// local/qwen3:8b).
func ValidateAIModel(ref string) error {
	provider, model, ok := strings.Cut(ref, "/")
	if !ok || model == "" {
		return fmt.Errorf("model %q: write provider/model, e.g. local/qwen3:8b or anthropic/claude-sonnet-5-5", ref)
	}
	if err := ValidateAIName("provider", provider); err != nil {
		return err
	}
	for _, c := range model {
		if c <= ' ' || c == '"' || c == '\\' || c == 0x7f {
			return fmt.Errorf("model %q: unexpected character", ref)
		}
	}
	return nil
}
