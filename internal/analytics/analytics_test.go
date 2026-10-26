package analytics

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func line(t time.Time, path string, status int, ms float64, ua, ip, country, ref string) string {
	return fmt.Sprintf(`{"level":"info","ts":%f,"logger":"http.log.access.app-web","msg":"handled request","duration":%f,"size":512,"status":%d,"request":{"remote_ip":"127.0.0.1","client_ip":%q,"method":"GET","host":"web.example.com","uri":%q,"headers":{"User-Agent":[%q],"Cf-Ipcountry":[%q],"Referer":[%q]}}}`+"\n",
		float64(t.UnixNano())/1e9, ms/1000, status, ip, path, ua, country, ref)
}

const (
	chrome = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"
	iphone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	gbot   = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
)

func TestParse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r, ok := Parse([]byte(line(now, "/api/chirps/42/like?x=1", 201, 12.5, iphone, "1.2.3.4", "GB", "https://news.ycombinator.com/item?id=1")))
	if !ok || r.Path != "/api/chirps/42/like" || r.Status != 201 || r.Ms < 12.4 || r.Ms > 12.6 || r.IP != "1.2.3.4" || r.Country != "GB" ||
		r.Referrer != "news.ycombinator.com" || r.Browser != "Safari" || !r.Mobile || r.Bot || !r.Time.Equal(now) {
		t.Fatalf("%+v", r)
	}
	// Same-site referrers aren't referrals; unknown countries are dropped.
	r, _ = Parse([]byte(line(now, "/", 200, 1, chrome, "1.2.3.4", "XX", "https://web.example.com/about")))
	if r.Referrer != "" || r.Country != "" || r.Browser != "Chrome" {
		t.Fatalf("%+v", r)
	}
	for ua, want := range map[string]string{gbot: "Crawler", "curl/8.7.1": "curl", "python-requests/2.32": "Script", "": "Unknown"} {
		if r, _ := Parse([]byte(line(now, "/", 200, 1, ua, "1.1.1.1", "", ""))); !r.Bot || r.Browser != want {
			t.Errorf("%q: %+v", ua, r)
		}
	}
	if _, ok := Parse([]byte("not json")); ok {
		t.Fatal("parsed junk")
	}
	if NormalizePath("/api/chirps/42/like") != "/api/chirps/:id/like" || NormalizePath("/u/3f2a9c1e-7d4b-4c1a-9e2f-0a1b2c3d4e5f") != "/u/:id" || NormalizePath("/about") != "/about" {
		t.Fatal("normalize")
	}
}

func TestQuery(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 29, 30, 0, time.UTC)
	s := NewStats()
	s.Now = func() time.Time { return now }
	add := func(ago time.Duration, path string, status int, ms float64, ua, ip string) {
		r, _ := Parse([]byte(line(now.Add(-ago), path, status, ms, ua, ip, "US", "")))
		s.Add("web", r)
	}
	for i := 0; i < 90; i++ {
		add(time.Duration(i)*time.Minute, "/", 200, 20, chrome, fmt.Sprint("10.0.0.", i%3))
	}
	for i := 0; i < 10; i++ {
		add(5*time.Minute, "/api/items/7", 500, 800, iphone, "10.0.0.9")
	}
	add(10*time.Minute, "/robots.txt", 404, 2, gbot, "66.249.1.1")
	add(8*24*time.Hour, "/", 200, 1, chrome, "10.0.0.1") // too old: ignored

	rep := s.Query("web", time.Hour, time.Minute, 5, true)
	if len(rep.Series) != 60 {
		t.Fatalf("points %d", len(rep.Series))
	}
	// 60 of the 90 steady requests are in the last hour (plus the burst and the bot).
	if rep.Totals.Req != 60+10+1 || rep.Totals.S5 != 10 || rep.Totals.S4 != 1 {
		t.Fatalf("totals %+v", rep.Totals)
	}
	if rep.Totals.Visitors != 4 || rep.Totals.Bots != 1 || rep.Totals.Mobile != 10 {
		t.Fatalf("people %+v", rep.Totals)
	}
	if rep.Totals.P50 < 10 || rep.Totals.P50 > 25 || rep.Totals.P95 < 500 || rep.Totals.P95 > 1000 {
		t.Fatalf("latency p50 %.1f p95 %.1f", rep.Totals.P50, rep.Totals.P95)
	}
	if rep.Paths[0] != (Count{"/", 60}) || rep.Paths[1] != (Count{"/api/items/:id", 10}) {
		t.Fatalf("paths %v", rep.Paths)
	}
	if rep.Browsers[0].Key != "Chrome" || rep.Countries[0] != (Count{"US", 71}) {
		t.Fatalf("browsers %v countries %v", rep.Browsers, rep.Countries)
	}
	if rep.Previous.Req != 30 {
		t.Fatalf("previous hour %d", rep.Previous.Req)
	}
	// Recent is in arrival order (logs are chronological), newest first, capped.
	if len(rep.Recent) != 100 || rep.Recent[0].Path != "/robots.txt" {
		t.Fatalf("recent %d %+v", len(rep.Recent), rep.Recent[0])
	}
	// The newest point is the current, partial minute.
	if last := rep.Series[len(rep.Series)-1]; last.T != now.Unix()/60*60 || last.Req != 1 {
		t.Fatalf("last point %+v", last)
	}

	// The live feed returns only what's new.
	_, seq := s.RecentSince("web", 0)
	add(0, "/new", 200, 3, chrome, "10.0.0.1")
	got, seq2 := s.RecentSince("web", seq)
	if len(got) != 1 || got[0].Path != "/new" || seq2 != seq+1 {
		t.Fatalf("since: %v %d", got, seq2)
	}

	// Several apps as one: a slow app pulls up the shared p95, and a
	// visitor to both counts once.
	for i := 0; i < 20; i++ {
		r, _ := Parse([]byte(line(now.Add(-time.Minute), "/slow", 200, 2000, chrome, "10.0.0.1", "US", "")))
		s.Add("api", r)
	}
	all := s.QueryAll([]string{"web", "api", "gone"}, time.Hour, time.Minute)
	if all.Totals.Req != 72+20 || all.Totals.Visitors != 4 || all.Totals.P95 < 1000 || len(all.Recent) != 0 {
		t.Fatalf("all apps %+v", all.Totals)
	}
	if web := s.Query("web", time.Hour, time.Minute, 0, false); web.Totals.P95 >= 1000 {
		t.Fatalf("web p95 %.0f picked up the other app", web.Totals.P95)
	}
}

func TestTailer(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cur := filepath.Join(dir, "access.x.log")
	os.WriteFile(cur, []byte(line(now, "/a", 200, 1, chrome, "1.1.1.1", "", "")+line(now, "/b", 200, 1, chrome, "1.1.1.1", "", "")), 0o600)
	s := NewStats()
	files := []string{cur}
	tl := NewTailer(s, func() map[string][]string { return map[string][]string{"web": files} })
	tl.Poll()
	if r := s.Query("web", time.Hour, time.Minute, 5, false); r.Totals.Req != 2 {
		t.Fatalf("first poll %d", r.Totals.Req)
	}
	// A half-written line waits; appended lines are read once.
	f, _ := os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0)
	l := line(now, "/c", 200, 1, chrome, "1.1.1.1", "", "")
	f.WriteString(l[:20])
	tl.Poll()
	f.WriteString(l[20:])
	f.Close()
	tl.Poll()
	tl.Poll()
	if r := s.Query("web", time.Hour, time.Minute, 5, false); r.Totals.Req != 3 {
		t.Fatalf("after append %d", r.Totals.Req)
	}
	// Rotation renames the file (same inode) and starts a new one: nothing
	// is counted twice.
	rotated := filepath.Join(dir, "access.x-2026-10-25T12-00-00.000.log")
	os.Rename(cur, rotated)
	os.WriteFile(cur, []byte(line(now, "/d", 200, 1, chrome, "1.1.1.1", "", "")), 0o600)
	files = []string{rotated, cur}
	tl.Poll()
	if r := s.Query("web", time.Hour, time.Minute, 5, false); r.Totals.Req != 4 {
		t.Fatalf("after rotation %d", r.Totals.Req)
	}
}
