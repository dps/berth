package analytics

import (
	"sort"
	"sync"
	"time"
)

// Keep is how much history the stats hold.
const Keep = 7 * 24 * time.Hour

const (
	block      = 10 * time.Minute // granularity of top lists and visitors
	recentKeep = 200
	topKeys    = 500 // per block; the rest count as "other"
)

// Latency histogram bucket upper bounds, in ms.
var latBounds = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

type minute struct {
	req   int
	class [6]int // 1xx..5xx at index 1..5
	bytes int64
	lat   [13]int // len(latBounds)+1
}

type blockStats struct {
	paths, referrers, countries, browsers map[string]int
	visitors                              map[uint64]struct{}
	bots, mobile                          int
}

func newBlock() *blockStats {
	return &blockStats{paths: map[string]int{}, referrers: map[string]int{}, countries: map[string]int{}, browsers: map[string]int{}, visitors: map[uint64]struct{}{}}
}

type appStats struct {
	minutes map[int64]*minute     // unix minute
	blocks  map[int64]*blockStats // unix time / 600
	recent  []Request             // newest last
	seq     int64                 // requests ever added, for the live feed
}

// Stats holds the last Keep of every app's requests, aggregated.
type Stats struct {
	mu   sync.Mutex
	apps map[string]*appStats
	Now  func() time.Time
}

func NewStats() *Stats { return &Stats{apps: map[string]*appStats{}, Now: time.Now} }

func (s *Stats) app(name string) *appStats {
	a := s.apps[name]
	if a == nil {
		a = &appStats{minutes: map[int64]*minute{}, blocks: map[int64]*blockStats{}}
		s.apps[name] = a
	}
	return a
}

func bump(m map[string]int, k string) {
	if _, ok := m[k]; !ok && len(m) >= topKeys {
		k = "(other)"
	}
	m[k]++
}

// Add counts one request.
func (s *Stats) Add(app string, r Request) {
	if s.Now().Sub(r.Time) > Keep {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.app(app)
	mk := r.Time.Unix() / 60
	m := a.minutes[mk]
	if m == nil {
		m = &minute{}
		a.minutes[mk] = m
	}
	m.req++
	if c := r.Status / 100; c >= 1 && c <= 5 {
		m.class[c]++
	}
	m.bytes += r.Bytes
	m.lat[sort.SearchFloat64s(latBounds, r.Ms)]++

	bk := r.Time.Unix() / int64(block/time.Second)
	b := a.blocks[bk]
	if b == nil {
		b = newBlock()
		a.blocks[bk] = b
	}
	if r.Bot {
		b.bots++
	} else {
		b.visitors[r.visitor] = struct{}{}
		if r.Mobile {
			b.mobile++
		}
	}
	bump(b.paths, NormalizePath(r.Path))
	if r.Referrer != "" {
		bump(b.referrers, r.Referrer)
	}
	if r.Country != "" {
		bump(b.countries, r.Country)
	}
	bump(b.browsers, r.Browser)

	a.seq++
	a.recent = append(a.recent, r)
	if len(a.recent) > recentKeep {
		a.recent = append(a.recent[:0:0], a.recent[len(a.recent)-recentKeep:]...)
	}
}

// Prune drops data older than Keep.
func (s *Stats) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := s.Now().Add(-Keep).Unix()
	for _, a := range s.apps {
		for k := range a.minutes {
			if k*60 < cut {
				delete(a.minutes, k)
			}
		}
		for k := range a.blocks {
			if (k+1)*int64(block/time.Second) < cut {
				delete(a.blocks, k)
			}
		}
	}
}

// Forget drops an app (destroyed).
func (s *Stats) Forget(app string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.apps, app)
}

// Point is one step of a time series.
type Point struct {
	T     int64   `json:"t"` // unix seconds, start of the step
	Req   int     `json:"req"`
	S2    int     `json:"s2"` // 1xx–3xx count as fine
	S4    int     `json:"s4"`
	S5    int     `json:"s5"`
	Bytes int64   `json:"bytes"`
	P50   float64 `json:"p50,omitempty"` // ms
	P95   float64 `json:"p95,omitempty"`
}

type Count struct {
	Key string `json:"key"`
	N   int    `json:"n"`
}

type Totals struct {
	Req      int     `json:"req"`
	S4       int     `json:"s4"`
	S5       int     `json:"s5"`
	Bytes    int64   `json:"bytes"`
	P50      float64 `json:"p50"`
	P95      float64 `json:"p95"`
	P99      float64 `json:"p99"`
	Visitors int     `json:"visitors"`
	Bots     int     `json:"bots"`
	Mobile   int     `json:"mobile"` // requests from phones and tablets (people only)
}

// Report is what the admin page draws for one app and range.
type Report struct {
	From      int64     `json:"from"`
	To        int64     `json:"to"`
	Step      int64     `json:"step"` // seconds
	Totals    Totals    `json:"totals"`
	Previous  Totals    `json:"previous"` // the same length of time just before
	Series    []Point   `json:"series"`
	Paths     []Count   `json:"paths"`
	Referrers []Count   `json:"referrers"`
	Countries []Count   `json:"countries"`
	Browsers  []Count   `json:"browsers"`
	Recent    []Request `json:"recent,omitempty"`
	Seq       int64     `json:"seq"`
}

// Query reports on app between to-span and to, in steps.
func (s *Stats) Query(app string, span, step time.Duration, top int, recent bool) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[app]
	if a == nil {
		a = &appStats{}
	}
	return s.query([]*appStats{a}, span, step, top, recent)
}

// QueryAll reports on several apps as one: latency percentiles come from
// their merged histograms, and a visitor to two apps counts once.
func (s *Stats) QueryAll(apps []string, span, step time.Duration) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	var as []*appStats
	for _, name := range apps {
		if a := s.apps[name]; a != nil {
			as = append(as, a)
		}
	}
	return s.query(as, span, step, 0, false)
}

func (s *Stats) query(as []*appStats, span, step time.Duration, top int, recent bool) *Report {
	now := s.Now()
	stepS := int64(step / time.Second)
	to := (now.Unix()/stepS + 1) * stepS // the current step, partial, is the last point
	from := to - int64(span/time.Second)
	rep := &Report{From: from, To: to, Step: stepS, Series: []Point{}, Paths: []Count{}, Referrers: []Count{}, Countries: []Count{}, Browsers: []Count{}}
	for _, a := range as {
		rep.Seq += a.seq
	}
	n := int((to - from) / stepS)
	hists := make([][13]int, n)
	var total [13]int
	for i := 0; i < n; i++ {
		rep.Series = append(rep.Series, Point{T: from + int64(i)*stepS})
	}
	for _, a := range as {
		for k, m := range a.minutes {
			t := k * 60
			if t < from || t >= to {
				continue
			}
			i := int((t - from) / stepS)
			p := &rep.Series[i]
			p.Req += m.req
			p.S2 += m.class[1] + m.class[2] + m.class[3]
			p.S4 += m.class[4]
			p.S5 += m.class[5]
			p.Bytes += m.bytes
			for j, c := range m.lat {
				hists[i][j] += c
				total[j] += c
			}
			rep.Totals.Req += m.req
			rep.Totals.S4 += m.class[4]
			rep.Totals.S5 += m.class[5]
			rep.Totals.Bytes += m.bytes
		}
	}
	for i := range rep.Series {
		if rep.Series[i].Req > 0 {
			rep.Series[i].P50 = percentile(hists[i], 0.50)
			rep.Series[i].P95 = percentile(hists[i], 0.95)
		}
	}
	rep.Totals.P50, rep.Totals.P95, rep.Totals.P99 = percentile(total, 0.5), percentile(total, 0.95), percentile(total, 0.99)

	maps := [4]map[string]int{{}, {}, {}, {}}
	visitors := map[uint64]struct{}{}
	bs := int64(block / time.Second)
	for _, a := range as {
		for k, b := range a.blocks {
			if (k+1)*bs <= from || k*bs >= to { // blocks overlapping the range
				continue
			}
			for i, m := range []map[string]int{b.paths, b.referrers, b.countries, b.browsers} {
				for key, c := range m {
					maps[i][key] += c
				}
			}
			for v := range b.visitors {
				visitors[v] = struct{}{}
			}
			rep.Totals.Bots += b.bots
			rep.Totals.Mobile += b.mobile
		}
	}
	rep.Totals.Visitors = len(visitors)
	rep.Paths, rep.Referrers, rep.Countries, rep.Browsers = topN(maps[0], top), topN(maps[1], top), topN(maps[2], top), topN(maps[3], top)

	// The same span just before, for "vs previous" deltas.
	var prev [13]int
	pv := map[uint64]struct{}{}
	pfrom := from - (to - from)
	for _, a := range as {
		for k, m := range a.minutes {
			if t := k * 60; t >= pfrom && t < from {
				rep.Previous.Req += m.req
				rep.Previous.S4 += m.class[4]
				rep.Previous.S5 += m.class[5]
				rep.Previous.Bytes += m.bytes
				for j, c := range m.lat {
					prev[j] += c
				}
			}
		}
	}
	for _, a := range as {
		for k, b := range a.blocks {
			if (k+1)*bs > pfrom && k*bs < from {
				for v := range b.visitors {
					pv[v] = struct{}{}
				}
			}
		}
	}
	rep.Previous.Visitors = len(pv)
	rep.Previous.P50, rep.Previous.P95, rep.Previous.P99 = percentile(prev, 0.5), percentile(prev, 0.95), percentile(prev, 0.99)

	if recent && len(as) == 1 { // only for a single app
		a := as[0]
		for i := len(a.recent) - 1; i >= 0 && len(rep.Recent) < 100; i-- {
			rep.Recent = append(rep.Recent, a.recent[i])
		}
	}
	return rep
}

// RecentSince returns requests added after seq (newest first), for the
// live feed, and the current seq.
func (s *Stats) RecentSince(app string, seq int64) ([]Request, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[app]
	if a == nil {
		return []Request{}, 0
	}
	n := int(a.seq - seq)
	if n > len(a.recent) || seq <= 0 {
		n = min(len(a.recent), 100)
	}
	out := make([]Request, 0, n)
	for i := len(a.recent) - 1; i >= len(a.recent)-n; i-- {
		out = append(out, a.recent[i])
	}
	return out, a.seq
}

// percentile reads a latency percentile off a histogram, interpolating
// linearly inside the bucket it falls in.
func percentile(h [13]int, q float64) float64 {
	total := 0
	for _, c := range h {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := q * float64(total)
	seen := 0.0
	for i, c := range h {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= rank {
			lo := 0.0
			if i > 0 {
				lo = latBounds[i-1]
			}
			hi := lo * 2
			if i < len(latBounds) {
				hi = latBounds[i]
			}
			return lo + (hi-lo)*(rank-seen)/float64(c)
		}
		seen += float64(c)
	}
	return latBounds[len(latBounds)-1]
}

func topN(m map[string]int, n int) []Count {
	out := make([]Count, 0, len(m))
	for k, c := range m {
		out = append(out, Count{k, c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
