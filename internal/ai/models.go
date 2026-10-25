// Package ai is Berth's AI gateway: one OpenAI-compatible endpoint, served
// by berthd, that every app can use for local models (Ollama on the Mac)
// and cloud providers, without ever holding a provider key itself.
package ai

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dps/berth/internal/state"
)

// Local is the provider name for models served by Ollama on the Mac.
const Local = "local"

// DefaultOllamaURL is where Ollama listens unless ai/config.json says
// otherwise.
const DefaultOllamaURL = "http://127.0.0.1:11434"

// Builtin are the cloud providers Berth knows, by their OpenAI-compatible
// base URLs. Others can be added with `berth ai:key <name> --url`.
var Builtin = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"anthropic":  "https://api.anthropic.com/v1",
	"gemini":     "https://generativelanguage.googleapis.com/v1beta/openai",
	"openrouter": "https://openrouter.ai/api/v1",
	"groq":       "https://api.groq.com/openai/v1",
	"mistral":    "https://api.mistral.ai/v1",
}

// AnthropicURL is the native Messages API, passed through at
// /anthropic/v1/messages for tools, caching and thinking.
var AnthropicURL = "https://api.anthropic.com"

// Target is a resolved model: which provider serves it and the model name
// that provider knows it by.
type Target struct {
	Provider string
	Model    string
}

func (t Target) String() string { return t.Provider + "/" + t.Model }

// Resolve turns what an app asked for (an alias or provider/model) into a
// target. Aliases point at provider/model, never at other aliases.
func Resolve(cfg *state.AIConfig, name string) (Target, error) {
	ref := name
	if to, ok := cfg.Aliases[name]; ok {
		ref = to
	}
	provider, model, ok := strings.Cut(ref, "/")
	if !ok || model == "" {
		known := make([]string, 0, len(cfg.Aliases))
		for a := range cfg.Aliases {
			known = append(known, a)
		}
		sort.Strings(known)
		hint := "no aliases are defined (berth ai:alias)"
		if len(known) > 0 {
			hint = "aliases: " + strings.Join(known, ", ")
		}
		return Target{}, fmt.Errorf("unknown model %q: use an alias or provider/model (%s)", name, hint)
	}
	if provider != Local && Builtin[provider] == "" && cfg.Providers[provider].URL == "" {
		return Target{}, fmt.Errorf("unknown provider %q in model %q", provider, name)
	}
	return Target{Provider: provider, Model: model}, nil
}

// BaseURL is the OpenAI-compatible base for a provider.
func BaseURL(cfg *state.AIConfig, provider string) string {
	if provider == Local {
		u := cfg.OllamaURL
		if u == "" {
			u = DefaultOllamaURL
		}
		return strings.TrimSuffix(u, "/") + "/v1"
	}
	if p, ok := cfg.Providers[provider]; ok {
		return strings.TrimSuffix(p.URL, "/")
	}
	return Builtin[provider]
}

// Allowed reports whether an app with this allow list may use what it
// asked for. An entry matches the name asked for (an alias), the resolved
// provider/model, or provider/* for any model of a provider.
func Allowed(allow []string, asked string, t Target) bool {
	if len(allow) == 0 {
		return true
	}
	return slices.Contains(allow, asked) || slices.Contains(allow, t.String()) || slices.Contains(allow, t.Provider+"/*")
}
