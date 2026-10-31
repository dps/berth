package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	userAgent     = "Mozilla/5.0 (compatible; BerthTools/1.0; +https://berth.computer)"
	fetchMaxBytes = 5 << 20
	fetchChars    = 20000 // what a fetch returns unless it asks for more
)

func init() {
	register(
		Provider{Name: "exa", Title: "Exa", SignUp: "https://exa.ai"},
		Provider{Name: "serpapi", Title: "SerpApi", SignUp: "https://serpapi.com"},
		Provider{Name: "direct", Title: "Berth", Note: "fetched by the Mac itself, public addresses only"},
	)
	define(&Tool{
		Name:    "web.search",
		Summary: "Search the web.",
		Input: []Field{
			{Name: "query", Type: "string", Required: true},
			{Name: "limit", Type: "integer", Min: 1, Max: 20, Desc: "Results to return, default 5."},
			{Name: "domains", Type: "array", Items: &Field{Type: "string"}, Desc: "Only search these sites, like [\"nytimes.com\"]."},
		},
		Output: []Field{{Name: "results", Type: "array", Items: &Field{Type: "object", Fields: []Field{
			{Name: "title", Type: "string"}, {Name: "url", Type: "string"}, {Name: "snippet", Type: "string"},
			{Name: "published", Type: "string", Desc: "When known."},
		}}}},
		Cache:   time.Hour,
		Example: `{"query":"mac mini","limit":1}`,
		Order:   []string{"exa", "serpapi"},
		Run:     map[string]RunFunc{"exa": exaSearch, "serpapi": serpSearch},
	})
	define(&Tool{
		Name:    "web.fetch",
		Summary: "Fetch a public web page and return its readable text.",
		Input: []Field{
			{Name: "url", Type: "string", Required: true, Desc: "An http or https URL on the public internet."},
			{Name: "max_chars", Type: "integer", Min: 1, Max: 200000, Desc: fmt.Sprintf("Longest text to return, default %d.", fetchChars)},
		},
		Output: []Field{
			{Name: "url", Type: "string", Desc: "The final URL, after redirects."}, {Name: "status", Type: "integer"},
			{Name: "content_type", Type: "string"}, {Name: "title", Type: "string"}, {Name: "text", Type: "string"},
			{Name: "truncated", Type: "boolean"},
		},
		Cache:   5 * time.Minute,
		Example: `{"url":"https://example.com","max_chars":200}`,
		Order:   []string{"direct"},
		Run:     map[string]RunFunc{"direct": webFetch},
	})
}

type searchResult struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"`
}

type searchResults struct {
	Results []searchResult `json:"results"`
}

func exaSearch(ctx context.Context, c *Call, args map[string]any) (any, error) {
	body := map[string]any{
		"query":      str(args, "query"),
		"numResults": intArg(args, "limit", 5),
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": 500}},
	}
	if d := strs(args, "domains"); d != nil {
		body["includeDomains"] = d
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("https://api.exa.ai/search"), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.Key)
	var r struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Text          string `json:"text"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := c.do(req, &r); err != nil {
		return nil, err
	}
	out := searchResults{[]searchResult{}}
	for _, x := range r.Results {
		out.Results = append(out.Results, searchResult{x.Title, x.URL, strings.Join(strings.Fields(x.Text), " "), x.PublishedDate})
	}
	return out, nil
}

func serpSearch(ctx context.Context, c *Call, args map[string]any) (any, error) {
	query := str(args, "query")
	if d := strs(args, "domains"); d != nil {
		query += " site:" + strings.Join(d, " OR site:")
	}
	limit := intArg(args, "limit", 5)
	q := url.Values{"engine": {"google"}, "q": {query}, "num": {strconv.Itoa(limit)}, "api_key": {c.Key}}
	var r struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
			Date    string `json:"date"`
		} `json:"organic_results"`
	}
	if err := c.get(ctx, "https://serpapi.com/search.json?"+q.Encode(), &r); err != nil {
		return nil, err
	}
	out := searchResults{[]searchResult{}}
	for _, x := range r.Organic {
		if len(out.Results) == limit {
			break
		}
		out.Results = append(out.Results, searchResult{x.Title, x.Link, x.Snippet, x.Date})
	}
	return out, nil
}

type fetched struct {
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Title       string `json:"title,omitempty"`
	Text        string `json:"text"`
	Truncated   bool   `json:"truncated"`
}

func webFetch(ctx context.Context, c *Call, args map[string]any) (any, error) {
	u, err := url.Parse(str(args, "url"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, badInput("url must be an http or https URL")
	}
	u.User = nil
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, badInput("url must be an http or https URL")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	resp, err := c.Public.Do(req)
	if err != nil {
		if errors.Is(err, errPrivate) {
			return nil, errorf(http.StatusForbidden, "private_address", "web.fetch only reaches the public internet")
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, errorf(http.StatusBadGateway, "fetch_failed", "couldn't fetch %s: %s", u.Host, err)
	}
	defer resp.Body.Close()

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	textual := strings.HasPrefix(mediaType, "text/") || mediaType == "application/xhtml+xml" || mediaType == "application/json" || mediaType == "application/xml" || mediaType == ""
	if !textual {
		return nil, errorf(http.StatusUnsupportedMediaType, "unsupported_type", "web.fetch reads text and HTML, and this is %s", mediaType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return nil, errorf(http.StatusBadGateway, "fetch_failed", "couldn't read %s: %s", u.Host, err)
	}
	out := fetched{URL: resp.Request.URL.String(), Status: resp.StatusCode, ContentType: mediaType}
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" || mediaType == "" {
		out.Title, out.Text = readable(body)
	} else {
		out.Text = strings.ToValidUTF8(string(body), "")
	}
	if limit := intArg(args, "max_chars", fetchChars); len(out.Text) > limit {
		r := []rune(out.Text)
		if len(r) > limit {
			out.Text, out.Truncated = string(r[:limit]), true
		}
	}
	return out, nil
}

var skipped = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true, "head": true, "iframe": true, "nav": true, "footer": true, "form": true}

var blocks = map[string]bool{"p": true, "div": true, "li": true, "tr": true, "br": true, "hr": true, "section": true, "article": true, "header": true, "main": true, "blockquote": true, "pre": true, "table": true, "ul": true, "ol": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true}

// readable pulls the title and visible text out of an HTML page.
func readable(page []byte) (title, text string) {
	var b strings.Builder
	z := html.NewTokenizer(bytes.NewReader(page))
	skip, inTitle := 0, false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return strings.TrimSpace(title), tidy(b.String())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			nameB, _ := z.TagName()
			name := string(nameB)
			if name == "title" {
				inTitle = tt == html.StartTagToken
			}
			if skipped[name] {
				if tt == html.StartTagToken {
					skip++
				} else if tt == html.EndTagToken && skip > 0 {
					skip--
				}
			}
			if blocks[name] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if inTitle && title == "" {
				title = string(z.Text())
			} else if skip == 0 {
				b.Write(z.Text())
			}
		}
	}
}

// tidy collapses the spaces and blank lines HTML leaves behind.
func tidy(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
