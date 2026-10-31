package state

import (
	"encoding/json"
	"os"
)

// ToolsConfig is the tools gateway's shared setup (design.md, Tools
// gateway), kept in tools/config.json. Provider keys live in
// secrets/tools.json.
type ToolsConfig struct {
	// Routes picks the providers behind a tool, first choice first. A tool
	// that isn't listed uses its built-in order.
	Routes map[string][]string `json:"routes,omitempty"`
	// Budgets caps a provider's calls per day, across every app that
	// shares its key.
	Budgets map[string]int `json:"budgets,omitempty"`
}

// AppTools is an app's access to the tools gateway.
type AppTools struct {
	TokenHash string `json:"token_hash"` // sha256 hex of the app's berth_tools_ token
	// Tools the app may call: names or patterns like web.*. Empty means all.
	Tools []string `json:"tools,omitempty"`
	// RPM caps calls per minute; 0 = no cap.
	RPM int `json:"rpm,omitempty"`
	// MonthlyCalls caps provider calls per calendar month (cache hits are
	// free); 0 = no cap.
	MonthlyCalls int64 `json:"monthly_calls,omitempty"`
	// KeylessOnly limits the app to providers that need no key, and to
	// answers already in the cache. Dev versions start this way.
	KeylessOnly bool `json:"keyless_only,omitempty"`
}

func (s *Store) ToolsConfig() (*ToolsConfig, error) {
	c := &ToolsConfig{}
	if err := readJSON(s.Path("tools", "config.json"), c); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if c.Routes == nil {
		c.Routes = map[string][]string{}
	}
	if c.Budgets == nil {
		c.Budgets = map[string]int{}
	}
	return c, nil
}

func (s *Store) SaveToolsConfig(c *ToolsConfig) error {
	if err := os.MkdirAll(s.Path("tools"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return WriteFile(s.Path("tools", "config.json"), append(b, '\n'), 0o600)
}

// ToolsConfigured reports whether the gateway has been used at all.
func (s *Store) ToolsConfigured() bool {
	_, err := os.Stat(s.Path("tools", "config.json"))
	return err == nil
}

func (s *Store) toolKeysPath() string { return s.Path("secrets", "tools.json") }

// ToolKeys is the provider keys, by provider name.
func (s *Store) ToolKeys() map[string]string {
	keys := map[string]string{}
	if b, err := os.ReadFile(s.toolKeysPath()); err == nil {
		json.Unmarshal(b, &keys)
	}
	return keys
}

// AIKeys is the AI gateway's provider keys, which some tools can share.
func (s *Store) AIKeys() map[string]string {
	keys := map[string]string{}
	if b, err := os.ReadFile(s.Path("secrets", "ai.json")); err == nil {
		json.Unmarshal(b, &keys)
	}
	return keys
}

func (s *Store) SaveToolKeys(keys map[string]string) error {
	b, _ := json.MarshalIndent(keys, "", "  ")
	return WriteFile(s.toolKeysPath(), append(b, '\n'), 0o600)
}
