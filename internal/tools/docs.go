package tools

import (
	"fmt"
	"strings"
)

// Info is a tool as an app sees it.
type Info struct {
	Name        string         `json:"name"`
	Summary     string         `json:"summary"`
	Path        string         `json:"path"`
	Input       map[string]any `json:"input"`
	Output      map[string]any `json:"output"`
	Providers   []string       `json:"providers"` // ready now, first choice first
	CacheSecond int            `json:"cache_seconds,omitempty"`
	Costly      bool           `json:"costly,omitempty"`
	Job         bool           `json:"job,omitempty"` // answers 202 with a job to check on
}

// Everyone is App's view of the whole catalog, for docs with no app.
func Everyone() App { return App{all: true} }

// Visible lists the tools an app may use, ready or not.
func Visible(app App, routes map[string][]string, keys map[string]string) []Info {
	var out []Info
	for _, t := range catalog {
		if !Allowed(app.Policy.Tools, t) && app.Name != Owner && !app.all {
			continue
		}
		route := Route(t, routes, keys)
		if app.Policy.KeylessOnly {
			route = keylessOnly(route)
		}
		out = append(out, Info{
			Name: t.Name, Summary: t.Summary, Path: t.Path(),
			Input: schema(t.Input), Output: schema(t.Output),
			Providers: append([]string{}, route...), CacheSecond: int(t.Cache.Seconds()), Costly: t.Costly, Job: t.Poll != nil,
		})
	}
	return out
}

func keylessOnly(route []string) []string {
	var out []string
	for _, n := range route {
		if providers[n].Keyless() {
			out = append(out, n)
		}
	}
	return out
}

func (g *Gateway) visible(app App) []Info {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return Visible(app, g.cfg.Routes, g.keys)
}

func (g *Gateway) listing(app App) map[string]any {
	return map[string]any{"tools": g.visible(app)}
}

func (g *Gateway) docs(app App) string { return Docs(g.visible(app)) }

func (g *Gateway) openAPI(app App) map[string]any { return OpenAPI(g.visible(app)) }

// OpenAPI describes tools as an OpenAPI 3.1 document. Paths are relative
// to BERTH_TOOLS_URL.
func OpenAPI(list []Info) map[string]any {
	paths := map[string]any{}
	for _, i := range list {
		paths[i.Path] = map[string]any{"post": map[string]any{
			"operationId": strings.ReplaceAll(i.Name, ".", "_"),
			"summary":     i.Summary,
			"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": i.Input}}},
			"responses": map[string]any{
				"200": map[string]any{"description": "The result", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"type": "object", "required": []string{"result", "meta"},
					"properties": map[string]any{"result": i.Output, "meta": map[string]any{"$ref": "#/components/schemas/Meta"}},
				}}}},
				"default": map[string]any{"description": "An error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}},
			},
		}}
	}
	return map[string]any{
		"openapi":  "3.1.0",
		"info":     map[string]any{"title": "Berth tools", "version": "1"},
		"security": []any{map[string]any{"bearer": []string{}}},
		"paths":    paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
			"schemas": map[string]any{
				"Meta": map[string]any{"type": "object", "properties": map[string]any{
					"tool": map[string]any{"type": "string"}, "provider": map[string]any{"type": "string"},
					"cached": map[string]any{"type": "boolean"}, "ms": map[string]any{"type": "integer"},
				}},
				"Error": map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "object", "properties": map[string]any{
					"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
				}}}},
			},
		},
	}
}

// Docs renders tools as Markdown, for coding agents and people.
func Docs(list []Info) string {
	var b strings.Builder
	b.WriteString("# Berth tools\n\n")
	b.WriteString("Every tool is a JSON POST to `$BERTH_TOOLS_URL` with `$BERTH_TOOLS_KEY` as a Bearer token. Provider keys stay in Berth.\n\n")
	b.WriteString("```bash\ncurl \"$BERTH_TOOLS_URL/v1/web/search\" -H \"Authorization: Bearer $BERTH_TOOLS_KEY\" \\\n  -d '{\"query\":\"mac mini\",\"limit\":3}'\n```\n\n")
	b.WriteString("A success is `{\"result\":…,\"meta\":{\"tool\",\"provider\",\"cached\",\"ms\"}}`. A failure is `{\"error\":{\"code\",\"message\"}}` with a matching HTTP status; 429 is a limit, 502 a provider failing, and 503 a tool with no provider set up. The gateway answers only inside Berth containers, so call it from server code.\n\n")
	b.WriteString("**Media.** Arguments that take a file accept a public URL, a `data:` URI, or the id of a file: upload one with `curl --data-binary @photo.jpg -H 'Content-Type: image/jpeg' \"$BERTH_TOOLS_URL/v1/files\"` plus the Authorization header, which answers `{\"file\":{\"id\",\"url\",…}}`. Tools that make a file return that same object. Fetch `url` with the Authorization header and save the bytes yourself: files last 24 hours, and browsers can't reach the gateway.\n\n")
	b.WriteString("**Jobs.** A tool marked as a job answers 202 with `{\"job\":{\"id\",\"status\":\"running\"}}`. Poll `GET $BERTH_TOOLS_URL/v1/jobs/<id>` every 10 seconds or so until `status` is `done`, with `result`, or `failed`, with `error`.\n\n")
	for _, i := range list {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n`POST %s`", i.Name, i.Summary, i.Path)
		if i.Job {
			b.WriteString(" (a job)")
		}
		if i.Costly {
			b.WriteString(" (costly: only for apps granted it by name)")
		}
		if len(i.Providers) == 0 {
			b.WriteString(" (not available yet: it needs a provider key)")
		}
		b.WriteString("\n\n")
		writeProps(&b, "Input", i.Input)
		writeProps(&b, "Result", i.Output)
	}
	return b.String()
}

func writeProps(b *strings.Builder, title string, s map[string]any) {
	props, _ := s["properties"].(map[string]any)
	if len(props) == 0 {
		return
	}
	required, _ := s["required"].([]string)
	fmt.Fprintf(b, "%s:\n\n", title)
	for _, name := range sortedNames(props) {
		p, _ := props[name].(map[string]any)
		note := ""
		for _, r := range required {
			if r == name {
				note = ", required"
			}
		}
		desc, _ := p["description"].(string)
		fmt.Fprintf(b, "- `%s` (%v%s)", name, p["type"], note)
		if desc != "" {
			fmt.Fprintf(b, ": %s", desc)
		}
		if e, ok := p["enum"].([]string); ok {
			fmt.Fprintf(b, " One of: %s.", strings.Join(e, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}
