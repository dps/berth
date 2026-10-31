package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dps/berth/internal/state"
)

// rig is a gateway with one app, and every provider host pointed at a fake.
type rig struct {
	g     *Gateway
	token string
	calls atomic.Int64
	srv   *httptest.Server
}

func newRig(t *testing.T, policy state.AppTools, cfg *state.ToolsConfig, keys map[string]string, fake http.HandlerFunc) *rig {
	t.Helper()
	r := &rig{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.calls.Add(1)
		fake(w, q)
	}))
	t.Cleanup(r.srv.Close)
	r.g = New(OpenUsage(t.TempDir()))
	r.g.Hosts = map[string]string{}
	for _, h := range []string{"api.exa.ai", "serpapi.com", "api.weatherapi.com", "api.open-meteo.com", "geocoding-api.open-meteo.com", "api.ebay.com",
		"generativelanguage.googleapis.com", "api.openai.com", "api.elevenlabs.io", "api.deepl.com", "api-free.deepl.com"} {
		r.g.Hosts[h] = r.srv.URL
	}
	r.g.AIURL, r.g.AIModel = r.srv.URL, "default"
	r.g.BaseURL = "http://gw:7790/tools"
	token, hash := NewToken()
	r.token = token
	policy.TokenHash = hash
	if cfg == nil {
		cfg = &state.ToolsConfig{}
	}
	r.g.Configure(cfg, keys, []App{{Name: "web", Policy: policy}})
	return r
}

type reply struct {
	status int
	Result map[string]any `json:"result"`
	Meta   Meta           `json:"meta"`
	Error  *Error         `json:"error"`
	raw    string
}

func (r *rig) post(t *testing.T, path, body string) reply {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.token)
	r.g.ServeHTTP(w, req)
	out := reply{status: w.Code, raw: w.Body.String()}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v: %s", path, err, w.Body)
	}
	return out
}

func TestAuth(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, nil, func(w http.ResponseWriter, _ *http.Request) {})
	w := httptest.NewRecorder()
	r.g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/web/fetch", strings.NewReader(`{}`)))
	if w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r.g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != 200 {
		t.Fatalf("healthz: %d", w.Code)
	}
}

func TestInputChecked(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, map[string]string{"exa": "k"}, func(w http.ResponseWriter, _ *http.Request) {})
	for body, want := range map[string]string{
		`{}`:                          "query is required",
		`{"query":"  "}`:              "query is required",
		`{"query":"x","limit":"3"}`:   "limit must be an integer",
		`{"query":"x","limit":2.5}`:   "limit must be an integer",
		`{"query":"x","limit":500}`:   "limit must be between",
		`{"query":"x","domains":[1]}`: "must be a string",
		`not json`:                    "must be a JSON object",
	} {
		got := r.post(t, "/v1/web/search", body)
		if got.status != 400 || !strings.Contains(got.Error.Message, want) {
			t.Errorf("%s: %d %s", body, got.status, got.raw)
		}
	}
	if r.calls.Load() != 0 {
		t.Fatal("a bad call reached the provider")
	}
}

func TestAllowList(t *testing.T) {
	r := newRig(t, state.AppTools{Tools: []string{"weather.*"}}, nil, nil, func(w http.ResponseWriter, _ *http.Request) {})
	if got := r.post(t, "/v1/web/fetch", `{"url":"https://example.com"}`); got.status != 403 || !strings.Contains(got.Error.Message, "tools:enable web") {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	if got := r.post(t, "/v1/nope/nope", `{}`); got.status != 404 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestNoProviderNamesTheFix(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, nil, func(w http.ResponseWriter, _ *http.Request) {})
	got := r.post(t, "/v1/web/search", `{"query":"x"}`)
	if got.status != 503 || !strings.Contains(got.Error.Message, "berth tools:key exa") {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestExaAndCacheSharedBetweenAppsAndArgOrder(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, map[string]string{"exa": "exa-secret"}, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path != "/search" || q.Header.Get("X-Api-Key") != "exa-secret" {
			t.Errorf("exa got %s key %q", q.URL.Path, q.Header.Get("X-Api-Key"))
		}
		var body map[string]any
		json.NewDecoder(q.Body).Decode(&body)
		if body["numResults"] != 3.0 || body["query"] != "asics" {
			t.Errorf("exa body %v", body)
		}
		io.WriteString(w, `{"results":[{"title":"A","url":"https://a.example","text":"  spread\n out ","publishedDate":"2026-10-01"}]}`)
	})
	a := r.post(t, "/v1/web/search", `{"query":"asics","limit":3}`)
	res := a.Result["results"].([]any)[0].(map[string]any)
	if a.status != 200 || a.Meta.Provider != "exa" || a.Meta.Cached || res["snippet"] != "spread out" || res["published"] != "2026-10-01" {
		t.Fatalf("%d %s", a.status, a.raw)
	}
	b := r.post(t, "/v1/web/search", `{"limit":3,"query":"asics","ignored":true}`)
	if !b.Meta.Cached || b.Meta.Provider != "exa" || r.calls.Load() != 1 {
		t.Fatalf("second call: %s, provider calls %d", b.raw, r.calls.Load())
	}
	if n := r.g.Usage.MonthCalls("web"); n != 1 {
		t.Fatalf("cache hit counted: %d", n)
	}
}

func TestFallbackAndBudget(t *testing.T) {
	cfg := &state.ToolsConfig{Budgets: map[string]int{"exa": 1}}
	var exa atomic.Int64
	r := newRig(t, state.AppTools{}, cfg, map[string]string{"exa": "e", "serpapi": "s"}, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/search" {
			exa.Add(1)
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"organic_results":[{"title":"B","link":"https://b.example","snippet":"s"},{"title":"C","link":"https://c.example"}]}`)
	})
	got := r.post(t, "/v1/web/search", `{"query":"one","limit":1}`)
	if got.status != 200 || got.Meta.Provider != "serpapi" || len(got.Result["results"].([]any)) != 1 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	// exa has spent its budget for today, so it's skipped.
	got = r.post(t, "/v1/web/search", `{"query":"two"}`)
	if got.Meta.Provider != "serpapi" || exa.Load() != 1 {
		t.Fatalf("exa calls %d: %s", exa.Load(), got.raw)
	}
}

func TestBudgetRefusesWhenNothingIsLeft(t *testing.T) {
	cfg := &state.ToolsConfig{Budgets: map[string]int{"exa": 1}}
	r := newRig(t, state.AppTools{}, cfg, map[string]string{"exa": "e"}, func(w http.ResponseWriter, q *http.Request) {
		io.WriteString(w, `{"results":[]}`)
	})
	r.post(t, "/v1/web/search", `{"query":"one"}`)
	got := r.post(t, "/v1/web/search", `{"query":"two"}`)
	if got.status != 429 || !strings.Contains(got.Error.Message, "tools:key exa --daily") {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestOwnerRouteChoosesProvider(t *testing.T) {
	cfg := &state.ToolsConfig{Routes: map[string][]string{"web.search": {"serpapi", "exa"}}}
	r := newRig(t, state.AppTools{}, cfg, map[string]string{"exa": "e", "serpapi": "s"}, func(w http.ResponseWriter, q *http.Request) {
		io.WriteString(w, `{"organic_results":[]}`)
	})
	if got := r.post(t, "/v1/web/search", `{"query":"x"}`); got.Meta.Provider != "serpapi" {
		t.Fatal(got.raw)
	}
}

func TestLimits(t *testing.T) {
	r := newRig(t, state.AppTools{MonthlyCalls: 1}, nil, map[string]string{"exa": "e"}, func(w http.ResponseWriter, q *http.Request) {
		io.WriteString(w, `{"results":[]}`)
	})
	r.post(t, "/v1/web/search", `{"query":"one"}`)
	if got := r.post(t, "/v1/web/search", `{"query":"one"}`); got.status != 200 || !got.Meta.Cached {
		t.Fatalf("a cache hit should pass a spent limit: %s", got.raw)
	}
	if got := r.post(t, "/v1/web/search", `{"query":"two"}`); got.status != 429 || got.Error.Code != "monthly_limit" {
		t.Fatalf("%d %s", got.status, got.raw)
	}

	r = newRig(t, state.AppTools{RPM: 1}, nil, map[string]string{"exa": "e"}, func(w http.ResponseWriter, q *http.Request) {
		io.WriteString(w, `{"results":[]}`)
	})
	r.post(t, "/v1/web/search", `{"query":"one"}`)
	if got := r.post(t, "/v1/web/search", `{"query":"one"}`); got.status != 429 || got.Error.Code != "rate_limit" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestKeylessOnly(t *testing.T) {
	r := newRig(t, state.AppTools{KeylessOnly: true}, nil, map[string]string{"exa": "e", "weatherapi": "w"}, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Query().Get("key") != "" {
			t.Error("a keyless app spent the weatherapi key")
		}
		switch q.URL.Path {
		case "/v1/search":
			io.WriteString(w, `{"results":[{"name":"Paris","admin1":"Île-de-France","country":"France","latitude":48.85,"longitude":2.35}]}`)
		default:
			io.WriteString(w, `{"current":{"temperature_2m":18.5,"relative_humidity_2m":60,"apparent_temperature":17,"precipitation":0,"weather_code":3,"wind_speed_10m":9}}`)
		}
	})
	got := r.post(t, "/v1/web/search", `{"query":"x"}`)
	if got.status != 403 || got.Error.Code != "keyless_only" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	got = r.post(t, "/v1/weather/current", `{"location":"Paris, France"}`)
	if got.status != 200 || got.Meta.Provider != "openmeteo" || got.Result["condition"] != "Overcast" || got.Result["temperature"] != 18.5 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestOpenMeteoRetriesThePlaceName(t *testing.T) {
	var names []string
	r := newRig(t, state.AppTools{}, nil, nil, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/v1/search" {
			names = append(names, q.URL.Query().Get("name"))
			if len(names) == 1 {
				io.WriteString(w, `{}`)
				return
			}
			io.WriteString(w, `{"results":[{"name":"San Francisco","latitude":37.77,"longitude":-122.41}]}`)
			return
		}
		if q.URL.Query().Get("temperature_unit") != "fahrenheit" || q.URL.Query().Get("forecast_days") != "2" {
			t.Errorf("forecast query %s", q.URL.RawQuery)
		}
		io.WriteString(w, `{"daily":{"time":["2026-10-30","2026-10-31"],"weather_code":[0,61],"temperature_2m_max":[70,65],"temperature_2m_min":[50,48],"precipitation_probability_max":[null,80]}}`)
	})
	got := r.post(t, "/v1/weather/forecast", `{"location":"San Francisco, CA","units":"imperial","days":2}`)
	days := got.Result["days"].([]any)
	if got.status != 200 || len(names) != 2 || names[1] != "San Francisco" || len(days) != 2 || days[1].(map[string]any)["condition"] != "Rain" {
		t.Fatalf("%v %s", names, got.raw)
	}
}

func TestUnknownPlaceDoesntFallBack(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, map[string]string{"weatherapi": "w"}, func(w http.ResponseWriter, q *http.Request) {
		w.WriteHeader(400)
	})
	if got := r.post(t, "/v1/weather/current", `{"location":"Nowhereville"}`); got.status != 404 || r.calls.Load() != 1 {
		t.Fatalf("%d after %d calls: %s", got.status, r.calls.Load(), got.raw)
	}
}

func TestKeysNeverReachTheApp(t *testing.T) {
	// A failing URL holds the key in its query string.
	r := newRig(t, state.AppTools{}, nil, map[string]string{"serpapi": "serp-secret"}, func(w http.ResponseWriter, q *http.Request) {
		w.WriteHeader(502)
		io.WriteString(w, "bad key serp-secret")
	})
	got := r.post(t, "/v1/web/search", `{"query":"x"}`)
	if got.status != 502 || strings.Contains(got.raw, "serp-secret") {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	r.srv.Close() // now the transport fails, and net/http's error names the URL
	got = r.post(t, "/v1/web/search", `{"query":"y"}`)
	if got.status != 502 || strings.Contains(got.raw, "serp-secret") {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestEbay(t *testing.T) {
	signIns := 0
	r := newRig(t, state.AppTools{}, nil, map[string]string{"ebay": "id-" + t.Name() + ":secret"}, func(w http.ResponseWriter, q *http.Request) {
		switch {
		case q.URL.Path == "/identity/v1/oauth2/token":
			signIns++
			if id, secret, _ := q.BasicAuth(); id != "id-"+t.Name() || secret != "secret" {
				t.Errorf("basic auth %q %q", id, secret)
			}
			io.WriteString(w, `{"access_token":"tok","expires_in":7200}`)
		case q.URL.Path == "/buy/browse/v1/item_summary/search":
			if q.Header.Get("Authorization") != "Bearer tok" || q.Header.Get("X-EBAY-C-MARKETPLACE-ID") != "EBAY_US" {
				t.Errorf("headers %v", q.Header)
			}
			want := "price:[10.00..],priceCurrency:USD,conditionIds:{1000|1500|1750},maxDeliveryCost:0"
			if q.URL.Query().Get("filter") != want || q.URL.Query().Get("sort") != "price" {
				t.Errorf("query %s", q.URL.RawQuery)
			}
			io.WriteString(w, `{"total":7,"itemSummaries":[{"itemId":"v1|1|0","title":"Asics","price":{"value":"59.99","currency":"USD"},
				"marketingPrice":{"originalPrice":{"value":"80.00","currency":"USD"}},"image":{"imageUrl":"https://i.example/1.jpg"},
				"condition":"New","itemWebUrl":"https://www.ebay.com/itm/1","seller":{"username":"shoes","feedbackPercentage":"99.5"},
				"shippingOptions":[{"shippingCost":{"value":"0.00","currency":"USD"}}],"buyingOptions":["FIXED_PRICE","AUCTION"],
				"itemLocation":{"city":"Reno","stateOrProvince":"NV","country":"US"},"categories":[{"categoryName":"Shoes"}]}]}`)
		case strings.HasPrefix(q.URL.Path, "/buy/browse/v1/item/"):
			if !strings.HasSuffix(q.URL.EscapedPath(), "v1%7C1%7C0") {
				t.Errorf("item path %s", q.URL.EscapedPath())
			}
			io.WriteString(w, `{"itemId":"v1|1|0","title":"Asics","price":{"value":"59.99","currency":"USD"},"itemWebUrl":"https://www.ebay.com/itm/1",
				"image":{"imageUrl":"https://i.example/1.jpg"},"additionalImages":[{"imageUrl":"https://i.example/2.jpg"}],
				"description":"<p>Great <b>shoes</b></p><script>x()</script>","localizedAspects":[{"name":"Brand","value":"ASICS"}],
				"returnTerms":{"returnsAccepted":true,"returnPeriod":{"value":30,"unit":"CALENDAR_DAY"}}}`)
		default:
			w.WriteHeader(404)
		}
	})
	got := r.post(t, "/v1/shopping/search", `{"query":"asics","min_price":10,"condition":"new","sort":"price_asc","free_shipping":true}`)
	if got.status != 200 || got.Result["total"] != 7.0 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	it := got.Result["items"].([]any)[0].(map[string]any)
	if it["price"] != 59.99 || it["original_price"] != 80.0 || it["shipping"] != 0.0 || it["auction"] != false ||
		it["location"] != "Reno, NV, US" || it["seller_feedback"] != 99.5 || it["category"] != "Shoes" {
		t.Fatalf("item %v", it)
	}
	got = r.post(t, "/v1/shopping/item", `{"id":"v1|1|0"}`)
	if got.status != 200 || got.Result["description"] != "Great shoes" || got.Result["returns"] != "30 day returns" ||
		len(got.Result["images"].([]any)) != 2 || got.Result["aspects"].(map[string]any)["Brand"] != "ASICS" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	if signIns != 1 {
		t.Fatalf("signed in %d times", signIns)
	}
}

func TestPublicAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.0.0.1", "192.168.66.1", "172.16.0.1", "169.254.169.254", "100.100.100.100", "0.0.0.0", "0.1.2.3", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "::ffff:192.168.1.1", "64:ff9b::7f00:1", "224.0.0.1", "240.0.0.1"} {
		if publicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s counted as public", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if !publicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s counted as private", a)
		}
	}
}

func TestWebFetchRefusesPrivateAddresses(t *testing.T) {
	reached := false
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { reached = true }))
	defer inside.Close()
	// A public-looking server that redirects inward has to fail the same way.
	r := newRig(t, state.AppTools{}, nil, nil, func(w http.ResponseWriter, q *http.Request) {})
	for _, u := range []string{inside.URL, "http://localhost:" + mustPort(t, inside.URL), "http://[::1]:7784/"} {
		got := r.post(t, "/v1/web/fetch", `{"url":"`+u+`"}`)
		if got.status != 403 || got.Error.Code != "private_address" {
			t.Fatalf("%s: %d %s", u, got.status, got.raw)
		}
	}
	if reached {
		t.Fatal("web.fetch reached a private address")
	}
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/", "not a url"} {
		if got := r.post(t, "/v1/web/fetch", `{"url":"`+u+`"}`); got.status != 400 {
			t.Fatalf("%s: %d %s", u, got.status, got.raw)
		}
	}
}

func TestWebFetchRedirectIntoThePrivateNetworkFails(t *testing.T) {
	client := publicClient()
	// Bypass the check for the first hop only: the redirect target still
	// goes through dialCheck.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		http.Redirect(w, q, "http://127.0.0.1:1/secret", http.StatusFound)
	}))
	defer first.Close()
	tr := client.Transport.(*http.Transport)
	check := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		if addr == first.Listener.Addr().String() {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return check(ctx, network, addr)
	}
	_, err := client.Get(first.URL)
	if !errors.Is(err, errPrivate) {
		t.Fatalf("redirect: %v", err)
	}
}

func TestWebFetchReadsPages(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/pic" {
			w.Header().Set("Content-Type", "image/png")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><title> The title </title><style>p{}</style></head><body><nav>menu</nav>
			<h1>Heading</h1><p>First   para.</p><script>alert(1)</script><p>Second &amp; last.</p></body></html>`)
	}))
	defer site.Close()
	r := newRig(t, state.AppTools{}, nil, nil, func(w http.ResponseWriter, q *http.Request) {})
	r.g.Public = site.Client() // httptest is on loopback, which the real client refuses
	got := r.post(t, "/v1/web/fetch", `{"url":"`+site.URL+`"}`)
	if got.status != 200 || got.Result["title"] != "The title" || got.Result["text"] != "Heading\nFirst para.\nSecond & last." {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	got = r.post(t, "/v1/web/fetch", `{"url":"`+site.URL+`/other","max_chars":7}`)
	if got.Result["text"] != "Heading" || got.Result["truncated"] != true {
		t.Fatalf("%s", got.raw)
	}
	if got = r.post(t, "/v1/web/fetch", `{"url":"`+site.URL+`/pic"}`); got.status != 415 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestListingAndDocsFollowThePolicy(t *testing.T) {
	r := newRig(t, state.AppTools{Tools: []string{"weather.*", "web.fetch"}}, nil, nil, func(w http.ResponseWriter, q *http.Request) {})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	r.g.ServeHTTP(w, req)
	var doc struct {
		OpenAPI string         `json:"openapi"`
		Paths   map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" || len(doc.Paths) != 3 || doc.Paths["/v1/web/fetch"] == nil || doc.Paths["/v1/web/search"] != nil {
		t.Fatalf("%s", w.Body)
	}
	docs := Docs(Visible(App{}, nil, nil))
	for _, want := range []string{"## web.search", "needs a provider key", "`POST /v1/shopping/search`", "`query` (string, required)"} {
		if !strings.Contains(docs, want) {
			t.Errorf("docs lack %q", want)
		}
	}
}

func TestUsageSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	u := OpenUsage(dir)
	u.Add(Record{Time: now, App: "web", Tool: "web.search", Provider: "exa", Status: 200, Ms: 10})
	u.Add(Record{Time: now, App: "web", Tool: "web.search", Provider: "exa", Status: 200, Cached: true})
	u.Add(Record{Time: now, App: "web", Tool: "web.search", Status: 403})
	u = OpenUsage(dir)
	if u.MonthCalls("web") != 1 || u.DayCalls("exa") != 1 {
		t.Fatalf("month %d day %d", u.MonthCalls("web"), u.DayCalls("exa"))
	}
	rows := u.Summary(month(now), "web")
	if len(rows) != 2 || rows[1].Calls != 2 || rows[1].Cached != 1 || rows[0].Errors != 1 {
		t.Fatalf("%+v", rows)
	}
}

func TestValidPattern(t *testing.T) {
	for _, ok := range []string{"web.*", "weather.current", "*"} {
		if err := ValidPattern(ok); err != nil {
			t.Error(err)
		}
	}
	for _, bad := range []string{"wbe.*", "[", "search"} {
		if ValidPattern(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEveryToolIsComplete(t *testing.T) {
	for _, tool := range Catalog() {
		if tool.Summary == "" || len(tool.Order) == 0 || len(tool.Run) != len(tool.Order) || len(strings.Split(tool.Name, ".")) != 2 {
			t.Errorf("%+v", tool)
		}
		for _, p := range tool.Order {
			if _, ok := providers[p]; !ok || tool.Run[p] == nil {
				t.Errorf("%s: provider %s", tool.Name, p)
			}
		}
	}
}

func mustPort(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestOwnerCanTestOneProvider(t *testing.T) {
	var keys []string
	r := newRig(t, state.AppTools{}, nil, map[string]string{"exa": "e", "serpapi": "s"}, func(w http.ResponseWriter, q *http.Request) {
		keys = append(keys, q.URL.Path)
		io.WriteString(w, `{"organic_results":[]}`)
	})
	r.g.SetAdmin("owner-token")
	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/web/search", strings.NewReader(`{"query":"x"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(ProviderHeader, "serpapi")
		w := httptest.NewRecorder()
		r.g.ServeHTTP(w, req)
		return w
	}
	for range 2 { // the second call skips the cache
		if w := send("owner-token"); w.Code != 200 || !strings.Contains(w.Body.String(), `"provider":"serpapi","cached":false`) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("provider calls %v", keys)
	}
	// An app can't pick a provider.
	if w := send(r.token); !strings.Contains(w.Body.String(), `"provider":"exa"`) {
		t.Fatalf("%s", w.Body)
	}
}

func TestEveryKeyedProviderCanBeTested(t *testing.T) {
	for _, n := range ProviderNames() {
		tool, ok := Sample(n)
		if !ok {
			t.Errorf("%s has no sample", n)
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(tool.Example), &args); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		if _, err := clean(tool.Input, args); err != nil {
			t.Errorf("%s example: %v", tool.Name, err)
		}
	}
}

func TestCostlyToolsNeedAGrant(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, map[string]string{"gemini": "g"}, func(w http.ResponseWriter, q *http.Request) {
		t.Error("the provider was called")
	})
	got := r.post(t, "/v1/video/generate", `{"prompt":"a boat"}`)
	if got.status != 403 || got.Error.Code != "not_allowed" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	video := Find("video.generate")
	if !Allowed([]string{"video.*"}, video) || !Allowed([]string{"*"}, video) || Allowed([]string{"web.*"}, video) {
		t.Fatal("an explicit grant should allow it, and only that")
	}
	for _, i := range Visible(App{}, nil, nil) {
		if i.Name == "video.generate" {
			t.Fatal("listed for an app without the grant")
		}
	}
}

func TestSoraVideoJob(t *testing.T) {
	polls := 0
	r := newRig(t, state.AppTools{Tools: []string{"*"}}, nil, map[string]string{"openai": "o"}, func(w http.ResponseWriter, q *http.Request) {
		switch q.Method + " " + q.URL.Path {
		case "POST /v1/videos":
			var b map[string]any
			json.NewDecoder(q.Body).Decode(&b)
			if b["size"] != "720x1280" || b["seconds"] != "8" || b["model"] != openAIVideoModel {
				t.Errorf("%v", b)
			}
			io.WriteString(w, `{"id":"video_1","status":"queued"}`)
		case "GET /v1/videos/video_1":
			polls++
			io.WriteString(w, `{"status":"completed"}`)
		case "GET /v1/videos/video_1/content":
			io.WriteString(w, "mp4")
		default:
			t.Errorf("unexpected %s %s", q.Method, q.URL.Path)
		}
	})
	got := r.post(t, "/v1/video/generate", `{"prompt":"a boat","aspect_ratio":"9:16","seconds":"8"}`)
	if got.status != 202 || got.Meta.Provider != "openai" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	var start struct {
		Job jobView `json:"job"`
	}
	json.Unmarshal([]byte(got.raw), &start)
	j, _ := r.g.jobs.load(start.Job.ID)
	j.Polled = time.Time{}
	r.g.jobs.save(j)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+start.Job.ID, nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	r.g.ServeHTTP(w, req)
	if polls != 1 || !strings.Contains(w.Body.String(), `"status":"done"`) || !strings.Contains(w.Body.String(), `"content_type":"video/mp4"`) {
		t.Fatalf("%d polls: %s", polls, w.Body)
	}
}

func TestVideoJobSurvivesARestart(t *testing.T) {
	polls := 0
	r := newRig(t, state.AppTools{Tools: []string{"*"}}, nil, map[string]string{"gemini": "video-key"}, func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("X-Goog-Api-Key") != "video-key" {
			t.Errorf("%s key %q", q.URL.Path, q.Header.Get("X-Goog-Api-Key"))
		}
		switch {
		case strings.HasSuffix(q.URL.Path, ":predictLongRunning"):
			io.WriteString(w, `{"name":"models/veo/operations/abc"}`)
		case strings.HasSuffix(q.URL.Path, "/operations/abc"):
			polls++
			if polls == 1 {
				io.WriteString(w, `{"done":false}`)
				return
			}
			io.WriteString(w, `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://generativelanguage.googleapis.com/v1beta/files/x:download"}}]}}}`)
		case strings.HasSuffix(q.URL.Path, ":download"):
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "fake video bytes")
		default:
			t.Errorf("unexpected %s", q.URL.Path)
		}
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/video/generate", strings.NewReader(`{"prompt":"a boat"}`))
	req.Header.Set("Authorization", "Bearer "+r.token)
	r.g.ServeHTTP(w, req)
	var start struct {
		Job jobView `json:"job"`
	}
	json.Unmarshal(w.Body.Bytes(), &start)
	if w.Code != 202 || start.Job.Status != "running" || strings.Contains(w.Body.String(), "operations/abc") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	backdate := func() {
		j, _ := r.g.jobs.load(start.Job.ID)
		j.Polled = time.Time{}
		r.g.jobs.save(j)
	}
	check := func(token string) (int, string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+start.Job.ID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.g.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	backdate()
	if code, body := check(r.token); code != 200 || !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("%d %s", code, body)
	}
	// Asking again at once doesn't reach the provider.
	check(r.token)
	if polls != 1 {
		t.Fatalf("polled %d times", polls)
	}
	// A new gateway on the same disk picks the job up: a restart.
	old := r.g
	r.g = New(old.Usage)
	r.g.Hosts, r.g.BaseURL = old.Hosts, old.BaseURL
	r.g.Configure(&state.ToolsConfig{}, map[string]string{"gemini": "video-key"}, []App{{Name: "web", Policy: state.AppTools{TokenHash: HashToken(r.token), Tools: []string{"*"}}}})
	backdate()
	code, body := check(r.token)
	if code != 200 || !strings.Contains(body, `"status":"done"`) || !strings.Contains(body, `"url":"http://gw:7790/tools/v1/files/file_`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := check("nope"); code != 401 {
		t.Fatal(code)
	}
}

func TestFilesBelongToTheirApp(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, nil, func(w http.ResponseWriter, q *http.Request) {})
	other, hash := NewToken()
	r.g.Configure(&state.ToolsConfig{}, nil, []App{
		{Name: "web", Policy: state.AppTools{TokenHash: HashToken(r.token)}},
		{Name: "other", Policy: state.AppTools{TokenHash: hash}},
	})
	do := func(method, path, token, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "image/png")
		r.g.ServeHTTP(w, req)
		return w
	}
	w := do("POST", "/v1/files", r.token, "pixels")
	var up struct {
		File FileRef `json:"file"`
	}
	json.Unmarshal(w.Body.Bytes(), &up)
	if w.Code != 200 || up.File.ContentType != "image/png" || up.File.Bytes != 6 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w = do("GET", "/v1/files/"+up.File.ID, r.token, ""); w.Code != 200 || w.Body.String() != "pixels" || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w = do("GET", "/v1/files/"+up.File.ID, other, ""); w.Code != 404 {
		t.Fatalf("another app read it: %d", w.Code)
	}
	if w = do("GET", "/v1/files/..%2f..%2fsecrets", r.token, ""); w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
	// One app can't pass another's file to a tool either.
	r.g.Configure(&state.ToolsConfig{}, map[string]string{"gemini": "g"}, []App{{Name: "other", Policy: state.AppTools{TokenHash: hash}}})
	if w = do("POST", "/v1/image/describe", other, `{"image":"`+up.File.ID+`"}`); w.Code != 404 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// Expired files are refused, then swept.
	dp, mp := r.g.Files.paths(up.File.ID)
	b, _ := os.ReadFile(mp)
	var m fileMeta
	json.Unmarshal(b, &m)
	m.Expires = time.Now().Add(-time.Minute)
	b, _ = json.Marshal(m)
	os.WriteFile(mp, b, 0o600)
	r.g.Files.sweep()
	if _, err := os.Stat(dp); err == nil {
		t.Fatal("expired file left behind")
	}
}

func TestMediaArgumentsCantReachTheMac(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, map[string]string{"gemini": "g"}, func(w http.ResponseWriter, q *http.Request) {
		t.Error("the provider was called")
	})
	got := r.post(t, "/v1/image/describe", `{"image":"http://127.0.0.1:2019/config/"}`)
	if got.status != 403 || got.Error.Code != "private_address" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	if got = r.post(t, "/v1/image/describe", `{"image":"file:///etc/passwd"}`); got.status != 400 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestImageGenerateStoresAFile(t *testing.T) {
	png := "cG5nIGJ5dGVz" // "png bytes"
	r := newRig(t, state.AppTools{}, nil, map[string]string{"gemini": "g", "openai": "o"}, func(w http.ResponseWriter, q *http.Request) {
		if strings.Contains(q.URL.Path, "generateContent") {
			w.WriteHeader(503)
			return
		}
		if q.URL.Path != "/v1/images/generations" || q.Header.Get("Authorization") != "Bearer o" {
			t.Errorf("%s %s", q.URL.Path, q.Header.Get("Authorization"))
		}
		var b map[string]any
		json.NewDecoder(q.Body).Decode(&b)
		if b["size"] != "1536x1024" {
			t.Errorf("size %v", b["size"])
		}
		io.WriteString(w, `{"data":[{"b64_json":"`+png+`"}]}`)
	})
	got := r.post(t, "/v1/image/generate", `{"prompt":"a boat","aspect_ratio":"16:9"}`)
	if got.status != 200 || got.Meta.Provider != "openai" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	f := got.Result["file"].(map[string]any)
	path, _, ok := r.g.Files.open("web", f["id"].(string))
	b, _ := os.ReadFile(path)
	if !ok || string(b) != "png bytes" || f["content_type"] != "image/png" {
		t.Fatalf("%v %q", f, b)
	}
}

func TestGeminiImageAndSafetyStop(t *testing.T) {
	var body map[string]any
	r := newRig(t, state.AppTools{}, nil, map[string]string{"gemini": "g"}, func(w http.ResponseWriter, q *http.Request) {
		json.NewDecoder(q.Body).Decode(&body)
		if strings.Contains(fmt.Sprint(body["contents"]), "blocked") {
			io.WriteString(w, `{"candidates":[{"finishReason":"SAFETY"}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"cGluZw=="}}]}}]}`)
	})
	if got := r.post(t, "/v1/image/edit", `{"prompt":"bluer","image":"data:image/png;base64,cGluZw==","aspect_ratio":"1:1"}`); got.status != 200 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
	if !strings.Contains(fmt.Sprint(body), "inlineData") || !strings.Contains(fmt.Sprint(body), "aspectRatio") {
		t.Fatalf("%v", body)
	}
	if got := r.post(t, "/v1/image/generate", `{"prompt":"blocked"}`); got.status != 400 || !strings.Contains(got.Error.Message, "safety") {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestSpeechVoicesMapToEachProvider(t *testing.T) {
	var seen []string
	r := newRig(t, state.AppTools{}, nil, map[string]string{"openai": "o", "elevenlabs": "e"}, func(w http.ResponseWriter, q *http.Request) {
		var b map[string]any
		json.NewDecoder(q.Body).Decode(&b)
		if q.URL.Path == "/v1/audio/speech" {
			seen = append(seen, fmt.Sprint(b["voice"]))
			w.WriteHeader(500)
			return
		}
		seen = append(seen, q.URL.Path)
		if q.Header.Get("Xi-Api-Key") != "e" {
			t.Errorf("key %q", q.Header.Get("Xi-Api-Key"))
		}
		io.WriteString(w, "mp3")
	})
	got := r.post(t, "/v1/audio/speak", `{"text":"Hi","voice":"male"}`)
	if got.status != 200 || got.Meta.Provider != "elevenlabs" || strings.Join(seen, " ") != "onyx /v1/text-to-speech/pNInz6obpgDQGcFmaJgB" {
		t.Fatalf("%v %s", seen, got.raw)
	}
	long := strings.Repeat("x", maxSpeak+1)
	if got = r.post(t, "/v1/audio/speak", `{"text":"`+long+`"}`); got.status != 400 {
		t.Fatalf("%d", got.status)
	}
}

func TestTranscribeSendsAFile(t *testing.T) {
	r := newRig(t, state.AppTools{}, nil, map[string]string{"openai": "o"}, func(w http.ResponseWriter, q *http.Request) {
		f, h, err := q.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		if string(b) != "ping" || h.Filename != "audio.wav" || q.FormValue("model") != openAITranscribe || q.FormValue("language") != "en" {
			t.Errorf("%q %s %s", b, h.Filename, q.FormValue("model"))
		}
		io.WriteString(w, `{"text":"hello there"}`)
	})
	got := r.post(t, "/v1/audio/transcribe", `{"audio":"data:audio/wav;base64,cGluZw==","language":"en"}`)
	if got.status != 200 || got.Result["text"] != "hello there" {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}

func TestTranslateUsesDeepLOrTheAIGateway(t *testing.T) {
	var path, auth string
	r := newRig(t, state.AppTools{}, nil, map[string]string{"deepl": "abc:fx", "ai": "gateway-token"}, func(w http.ResponseWriter, q *http.Request) {
		path, auth = q.URL.Path, q.Header.Get("Authorization")
		if q.URL.Path == "/v2/translate" {
			io.WriteString(w, `{"translations":[{"text":"Bonjour","detected_source_language":"EN"}]}`)
			return
		}
		var b map[string]any
		json.NewDecoder(q.Body).Decode(&b)
		if b["model"] != "default" {
			t.Errorf("model %v", b["model"])
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"<think>hmm</think>\n\nBonjour"}}]}`)
	})
	got := r.post(t, "/v1/text/translate", `{"text":"Hello","to":"fr"}`)
	if got.Result["text"] != "Bonjour" || got.Result["detected"] != "en" || auth != "DeepL-Auth-Key abc:fx" {
		t.Fatalf("%s %s", auth, got.raw)
	}
	cfg := &state.ToolsConfig{Routes: map[string][]string{"text.translate": {"ai"}}}
	r.g.Configure(cfg, map[string]string{"ai": "gateway-token"}, []App{{Name: "web", Policy: state.AppTools{TokenHash: HashToken(r.token)}}})
	got = r.post(t, "/v1/text/translate", `{"text":"Goodbye","to":"fr"}`)
	if got.Result["text"] != "Bonjour" || path != "/v1/chat/completions" || auth != "Bearer gateway-token" {
		t.Fatalf("%s %s %s", path, auth, got.raw)
	}
}

func TestKeylessAppsCantUseTheAIGateway(t *testing.T) {
	r := newRig(t, state.AppTools{KeylessOnly: true}, nil, map[string]string{"ai": "t"}, func(w http.ResponseWriter, q *http.Request) {
		t.Error("the gateway was called")
	})
	if got := r.post(t, "/v1/text/translate", `{"text":"Hi","to":"fr"}`); got.status != 403 {
		t.Fatalf("%d %s", got.status, got.raw)
	}
}
