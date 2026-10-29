// Command sitegen turns README.md into the documentation pages of
// berth.computer: one page per ## section, with the sidebar taken from the
// README's Contents section, so the README stays the one place the docs
// are written. Run `make site` from the repository root; it writes
// site/public/docs/ and site/public/sitemap.xml, which are committed.
package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

//go:embed page.html
var pageHTML string

type page struct {
	Title   string
	Slug    string // "" for the docs home
	Summary string
	Nodes   []ast.Node
	Subs    []sub
	Content template.HTML
}

type sub struct{ Title, ID string }

type group struct {
	Name  string
	Pages []*page
}

func (p *page) URL() string {
	if p.Slug == "" {
		return "/docs/"
	}
	return "/docs/" + p.Slug + "/"
}

func main() {
	readme := flag.String("readme", "README.md", "the README to read")
	out := flag.String("out", "site/public", "the site's public folder")
	base := flag.String("base", "https://berth.computer", "the site's address, for the sitemap")
	repo := flag.String("repo", "https://github.com/dps/berth", "the repository, for links to other files")
	flag.Parse()
	if err := run(*readme, *out, *base, *repo); err != nil {
		fmt.Fprintln(os.Stderr, "sitegen:", err)
		os.Exit(1)
	}
}

func run(readme, out, base, repo string) error {
	src, err := os.ReadFile(readme)
	if err != nil {
		return err
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
	ctx := parser.NewContext(parser.WithIDs(&githubIDs{seen: map[string]bool{}}))
	doc := md.Parser().Parse(text.NewReader(src), parser.WithContext(ctx))

	// Split the document at each ## heading. What comes before the first
	// is the README's banner, which the landing page covers.
	var pages []*page
	var contents *page
	var cur *page
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level == 2 {
			cur = &page{Title: plain(h, src), Slug: headingID(h)}
			if cur.Slug == "contents" {
				contents = cur
			} else {
				pages = append(pages, cur)
			}
			continue
		}
		if cur != nil {
			cur.Nodes = append(cur.Nodes, n)
		}
	}
	if contents == nil || len(pages) == 0 || pages[0].Slug != "why" {
		return fmt.Errorf("%s should start with ## Why, then ## Contents", readme)
	}
	pages[0].Slug = "" // Why is the docs home
	pages[0].Title = "Introduction"

	// Every heading's id, and the page it's on.
	anchors := map[string]*page{}
	for _, p := range pages {
		if p.Slug != "" {
			anchors[p.Slug] = p
		} else {
			anchors["why"] = p
		}
		for _, n := range p.Nodes {
			ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
				if h, ok := n.(*ast.Heading); ok && entering {
					anchors[headingID(h)] = p
				}
				return ast.WalkContinue, nil
			})
		}
	}

	groups, err := navigation(contents, anchors, src)
	if err != nil {
		return err
	}
	// Pages the Contents doesn't list (the license) go last.
	listed := map[*page]bool{pages[0]: true}
	for _, g := range groups {
		for _, p := range g.Pages {
			listed[p] = true
		}
	}
	var more group
	for _, p := range pages {
		if !listed[p] {
			more.Pages = append(more.Pages, p)
		}
	}
	if len(more.Pages) > 0 {
		more.Name = "More"
		groups = append(groups, more)
	}

	var bad []string
	for _, p := range pages {
		for len(p.Nodes) > 0 && p.Nodes[len(p.Nodes)-1].Kind() == ast.KindThematicBreak {
			p.Nodes = p.Nodes[:len(p.Nodes)-1]
		}
		for _, n := range p.Nodes {
			ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
				if !entering {
					return ast.WalkContinue, nil
				}
				switch n := n.(type) {
				case *ast.Heading:
					// ### becomes the page's second level, under its title.
					if n.Level > 2 {
						if n.Level == 3 {
							p.Subs = append(p.Subs, sub{plain(n, src), headingID(n)})
						}
						n.Level--
					}
				case *ast.Link:
					dest, err := rewrite(string(n.Destination), anchors, repo, "blob")
					if err != nil {
						bad = append(bad, fmt.Sprintf("%s: %v", p.Title, err))
					}
					n.Destination = []byte(dest)
				case *ast.Image:
					dest, _ := rewrite(string(n.Destination), anchors, repo, "raw")
					n.Destination = []byte(dest)
				}
				return ast.WalkContinue, nil
			})
		}
		for _, n := range p.Nodes {
			if para, ok := n.(*ast.Paragraph); ok && p.Summary == "" {
				if t := plain(para, src); !strings.HasSuffix(t, ":") {
					p.Summary = firstSentence(t)
				}
			}
		}
		var b bytes.Buffer
		for _, n := range p.Nodes {
			if err := md.Renderer().Render(&b, src, n); err != nil {
				return err
			}
		}
		p.Content = template.HTML(b.String())
	}
	if len(bad) > 0 {
		return fmt.Errorf("links that go nowhere:\n  %s", strings.Join(bad, "\n  "))
	}

	tmpl, err := template.New("page").Parse(pageHTML)
	if err != nil {
		return err
	}
	docs := filepath.Join(out, "docs")
	if err := os.RemoveAll(docs); err != nil {
		return err
	}
	order := []*page{pages[0]}
	for _, g := range groups {
		order = append(order, g.Pages...)
	}
	for i, p := range order {
		data := map[string]any{
			"Page": p, "Groups": groups, "Home": pages[0],
			"Edit": repo + "/edit/main/README.md",
		}
		if i > 0 {
			data["Prev"] = order[i-1]
		}
		if i+1 < len(order) {
			data["Next"] = order[i+1]
		}
		if p.Slug == "" {
			data["Index"] = groups
		}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, data); err != nil {
			return err
		}
		dir := filepath.Join(docs, p.Slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), b.Bytes(), 0o644); err != nil {
			return err
		}
	}

	var sm strings.Builder
	sm.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\">\n")
	fmt.Fprintf(&sm, "  <url><loc>%s/</loc></url>\n", base)
	for _, p := range order {
		fmt.Fprintf(&sm, "  <url><loc>%s%s</loc></url>\n", base, p.URL())
	}
	sm.WriteString("</urlset>\n")
	return os.WriteFile(filepath.Join(out, "sitemap.xml"), []byte(sm.String()), 0o644)
}

// navigation reads the README's Contents section: paragraphs that start
// with a bold group name and list links to sections.
func navigation(contents *page, anchors map[string]*page, src []byte) ([]group, error) {
	var groups []group
	for _, n := range contents.Nodes {
		para, ok := n.(*ast.Paragraph)
		if !ok {
			continue
		}
		var g group
		for c := para.FirstChild(); c != nil; c = c.NextSibling() {
			switch c := c.(type) {
			case *ast.Emphasis:
				if c.Level == 2 && g.Name == "" {
					g.Name = strings.TrimSuffix(strings.TrimSpace(plain(c, src)), ":")
				}
			case *ast.Link:
				id := strings.TrimPrefix(string(c.Destination), "#")
				p, ok := anchors[id]
				if !ok || p.Slug != id {
					return nil, fmt.Errorf("Contents links to #%s, which isn't a ## section", id)
				}
				g.Pages = append(g.Pages, p)
			}
		}
		if g.Name != "" && len(g.Pages) > 0 {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("found no groups in the Contents section")
	}
	return groups, nil
}

// rewrite points a README link at the site: #anchors at the page that has
// them, other files in the repository at GitHub.
func rewrite(dest string, anchors map[string]*page, repo, kind string) (string, error) {
	switch {
	case dest == "" || strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:"):
		return dest, nil
	case strings.HasPrefix(dest, "#"):
		id := dest[1:]
		p, ok := anchors[id]
		if !ok {
			return dest, fmt.Errorf("%s", dest)
		}
		if id == p.Slug || (p.Slug == "" && id == "why") {
			return p.URL(), nil
		}
		return p.URL() + "#" + id, nil
	default:
		path, frag, _ := strings.Cut(strings.TrimPrefix(dest, "./"), "#")
		u := repo + "/" + kind + "/main/" + path
		if frag != "" {
			u += "#" + frag
		}
		return u, nil
	}
}

func headingID(h *ast.Heading) string {
	if v, ok := h.AttributeString("id"); ok {
		if b, ok := v.([]byte); ok {
			return string(b)
		}
	}
	return ""
}

// plain is a node's text with the markup taken out.
func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if r := []rune(s); len(r) > 160 {
		s = strings.TrimSpace(string(r[:157])) + "…"
	}
	return s
}

// githubIDs makes heading ids the way GitHub does, so the README's own
// #links work on the site: lower case, punctuation dropped, spaces to
// dashes, and -1, -2 on repeats.
type githubIDs struct{ seen map[string]bool }

func (g *githubIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	var b strings.Builder
	for _, r := range strings.ToLower(string(value)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	id := b.String()
	for i, base := 1, id; g.seen[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	g.seen[id] = true
	return []byte(id)
}

func (g *githubIDs) Put(value []byte) { g.seen[string(value)] = true }
