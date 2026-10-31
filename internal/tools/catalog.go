// Package tools is the tools gateway (design.md, Tools gateway): one
// service on the Mac that holds provider keys and gives every app the same
// plain HTTP tools, like web.search and weather.forecast.
//
// Apps name capabilities, not vendors. Each tool is defined once here, with
// the providers that can serve it; the HTTP API, the OpenAPI document and
// the docs all come from that definition.
package tools

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// Field describes one input argument or output value.
type Field struct {
	Name     string
	Type     string // string, integer, number, boolean, array or object
	Desc     string
	Required bool
	Enum     []string
	Min, Max float64 // for integers and numbers; both zero means no range
	Items    *Field  // for arrays
	Fields   []Field // for objects
}

// Provider is a service that can serve tools.
type Provider struct {
	Name    string
	Title   string
	SignUp  string // where to get a key; empty for keyless providers
	KeyHint string // the key's format, when it isn't one plain string
	Note    string
	// Internal is served by Berth itself but isn't free to use, like a
	// model behind the AI gateway. It takes no key of its own.
	Internal bool
}

// Keyless reports whether the provider works without a key.
func (p Provider) Keyless() bool { return p.SignUp == "" && !p.Internal }

// RunFunc calls one provider. An *Error is final, because trying another
// provider wouldn't help; any other error moves on to the next provider.
type RunFunc func(ctx context.Context, c *Call, args map[string]any) (any, error)

// PollFunc checks a provider's long-running operation. It returns a nil
// result while the operation is still running.
type PollFunc func(ctx context.Context, c *Call, op string) (any, error)

// Pending is what a RunFunc returns for work that takes minutes: the
// provider's name for the operation, which outlives a restart.
type Pending struct{ Op string }

// Tool is one capability. Run holds a function for each provider, and
// Order is which is tried first when the owner hasn't chosen.
type Tool struct {
	Name    string // group.tool
	Summary string
	Input   []Field
	Output  []Field
	Cache   time.Duration // how long answers are shared between apps; 0 = never
	Example string        // JSON arguments, for testing a provider's key
	Timeout time.Duration // how long one provider call may take; 0 = the default
	// Costly tools can run up a real bill in a few calls, so an app only
	// gets them by name: an empty allow list leaves them out.
	Costly bool
	Order  []string
	Run    map[string]RunFunc
	// Poll, for tools whose Run returns Pending, checks on a running job.
	Poll map[string]PollFunc
}

// Path is the tool's URL path under /v1.
func (t *Tool) Path() string { return "/v1/" + strings.Replace(t.Name, ".", "/", 1) }

var (
	providers = map[string]Provider{}
	catalog   []*Tool
)

func register(p ...Provider) {
	for _, x := range p {
		providers[x.Name] = x
	}
}

func define(t *Tool) {
	catalog = append(catalog, t)
	slices.SortFunc(catalog, func(a, b *Tool) int { return strings.Compare(a.Name, b.Name) })
}

// Catalog lists every tool, by name.
func Catalog() []*Tool { return catalog }

// Find returns a tool by name.
func Find(name string) *Tool {
	for _, t := range catalog {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// FindProvider returns a provider by name.
func FindProvider(name string) (Provider, bool) {
	p, ok := providers[name]
	return p, ok
}

// ProviderNames lists the providers that take a key, sorted.
func ProviderNames() []string {
	var out []string
	for n, p := range providers {
		if !p.Keyless() && !p.Internal {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// AllProviders lists every provider, by name.
func AllProviders() []Provider {
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Provider) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ToolsOf lists the tools a provider can serve.
func ToolsOf(provider string) []string {
	var out []string
	for _, t := range catalog {
		if _, ok := t.Run[provider]; ok {
			out = append(out, t.Name)
		}
	}
	return out
}

// Sample is the tool and arguments that test a provider's key.
func Sample(provider string) (*Tool, bool) {
	for _, t := range catalog {
		if _, ok := t.Run[provider]; ok && t.Example != "" {
			return t, true
		}
	}
	return nil, false
}

// Allowed reports whether an app with these patterns may call the tool.
func Allowed(patterns []string, t *Tool) bool {
	if len(patterns) == 0 {
		return !t.Costly
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, t.Name); ok {
			return true
		}
	}
	return false
}

// ValidPattern checks a --tools pattern matches at least one tool, so a
// typo fails when it's set rather than on the app's first call.
func ValidPattern(p string) error {
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("bad tool pattern %q", p)
	}
	for _, t := range catalog {
		if ok, _ := path.Match(p, t.Name); ok {
			return nil
		}
	}
	return fmt.Errorf("no tool matches %q (see berth tools)", p)
}

// Route is the providers to try for a tool, first choice first: the
// owner's order if there is one, keeping only those that are ready.
func Route(t *Tool, routes map[string][]string, keys map[string]string) []string {
	order := t.Order
	if r := routes[t.Name]; len(r) > 0 {
		order = r
	}
	var out []string
	for _, n := range order {
		p, ok := providers[n]
		if _, runs := t.Run[n]; ok && runs && (p.Keyless() || keys[n] != "") {
			out = append(out, n)
		}
	}
	return out
}

// schema renders fields as a JSON Schema object.
func schema(fields []Field) map[string]any {
	props := map[string]any{}
	var required []string
	for _, f := range fields {
		props[f.Name] = fieldSchema(f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props}
	if required != nil {
		s["required"] = required
	}
	return s
}

func fieldSchema(f Field) map[string]any {
	s := map[string]any{"type": f.Type}
	if f.Type == "object" {
		s = schema(f.Fields)
	}
	if f.Desc != "" {
		s["description"] = f.Desc
	}
	if f.Enum != nil {
		s["enum"] = f.Enum
	}
	if f.Min != 0 || f.Max != 0 {
		s["minimum"], s["maximum"] = f.Min, f.Max
	}
	if f.Items != nil {
		s["items"] = fieldSchema(*f.Items)
	}
	return s
}

// clean checks args against the tool's input and drops anything else, so
// a provider only ever sees what the tool declares.
func clean(fields []Field, args map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range fields {
		v, ok := args[f.Name]
		if !ok || v == nil {
			if f.Required {
				return nil, fmt.Errorf("%s is required", f.Name)
			}
			continue
		}
		if err := check(f, v); err != nil {
			return nil, err
		}
		out[f.Name] = v
	}
	return out, nil
}

func check(f Field, v any) error {
	article := "a"
	if f.Type == "integer" || f.Type == "array" || f.Type == "object" {
		article = "an"
	}
	bad := fmt.Errorf("%s must be %s %s", f.Name, article, f.Type)
	switch f.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return bad
		}
		if f.Required && strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", f.Name)
		}
		if f.Enum != nil && !slices.Contains(f.Enum, s) {
			return fmt.Errorf("%s must be one of %s", f.Name, strings.Join(f.Enum, ", "))
		}
	case "integer", "number":
		n, ok := v.(float64)
		if !ok || f.Type == "integer" && n != float64(int64(n)) {
			return bad
		}
		if (f.Min != 0 || f.Max != 0) && (n < f.Min || n > f.Max) {
			return fmt.Errorf("%s must be between %v and %v", f.Name, f.Min, f.Max)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad
		}
	case "array":
		items, ok := v.([]any)
		if !ok {
			return bad
		}
		if f.Items != nil {
			for _, x := range items {
				if err := check(*f.Items, x); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func str(args map[string]any, name string) string {
	s, _ := args[name].(string)
	return strings.TrimSpace(s)
}

func intArg(args map[string]any, name string, def int) int {
	if n, ok := args[name].(float64); ok {
		return int(n)
	}
	return def
}

// short checks a text argument's length.
func short(args map[string]any, name string, limit int) error {
	if n := len([]rune(str(args, name))); n > limit {
		return badInput("%s is %d characters; the limit is %d", name, n, limit)
	}
	return nil
}

func strs(args map[string]any, name string) []string {
	var out []string
	items, _ := args[name].([]any)
	for _, x := range items {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
