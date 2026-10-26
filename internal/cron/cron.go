// Package cron parses the five-field schedules of app.json's "cron" entries
// (Dokku's format) and says when they fire. Times are the Mac's local time.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression: one bit per allowed value.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar: when both day fields are restricted, a day
	// matching either counts (classic cron).
	domStar, dowStar bool
}

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Parse reads "m h dom mon dow" or a macro such as @daily.
func Parse(spec string) (*Schedule, error) {
	spec = strings.TrimSpace(spec)
	if m, ok := macros[strings.ToLower(spec)]; ok {
		spec = m
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return nil, fmt.Errorf("schedule %q: want five fields (minute hour day month weekday) or @hourly, @daily, @weekly, @monthly", spec)
	}
	s := &Schedule{domStar: f[2] == "*", dowStar: f[4] == "*"}
	var err error
	for _, p := range []struct {
		dst      *uint64
		field    string
		lo, hi   int
		names    []string
		nameBase int
	}{
		{&s.minute, f[0], 0, 59, nil, 0},
		{&s.hour, f[1], 0, 23, nil, 0},
		{&s.dom, f[2], 1, 31, nil, 0},
		{&s.month, f[3], 1, 12, monthNames, 1},
		{&s.dow, f[4], 0, 7, dayNames, 0},
	} {
		if *p.dst, err = field(p.field, p.lo, p.hi, p.names, p.nameBase); err != nil {
			return nil, fmt.Errorf("schedule %q: %w", spec, err)
		}
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	return s, nil
}

func field(f string, lo, hi int, names []string, nameBase int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			step = n
		}
		a, b := lo, hi
		if rng != "*" {
			from, to, isRange := strings.Cut(rng, "-")
			var err error
			if a, err = value(from, lo, hi, names, nameBase); err != nil {
				return 0, err
			}
			b = a
			if isRange {
				if b, err = value(to, lo, hi, names, nameBase); err != nil {
					return 0, err
				}
			} else if hasStep {
				b = hi // "5/15" means from 5 to the end, every 15
			}
			if b < a {
				return 0, fmt.Errorf("range %q runs backwards", rng)
			}
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func value(s string, lo, hi int, names []string, nameBase int) (int, error) {
	for i, n := range names {
		if strings.EqualFold(s, n) {
			return i + nameBase, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%q isn't between %d and %d", s, lo, hi)
	}
	return v, nil
}

// Matches reports whether the schedule fires in t's minute.
func (s *Schedule) Matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 || s.hour&(1<<uint(t.Hour())) == 0 || s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	return s.dayMatches(t)
}

// Next is the first minute after t when the schedule fires, or the zero
// time if it never does within five years (say, February 30th).
func (s *Schedule) Next(t time.Time) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, t.Location())
	for end := t.AddDate(5, 0, 0); t.Before(end); {
		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case s.hour&(1<<uint(t.Hour())) == 0:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
		case s.minute&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}
