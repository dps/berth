package cron

import (
	"testing"
	"time"
)

func TestParseAndNext(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	from := at("2026-10-25 10:17") // a Sunday
	for spec, want := range map[string]string{
		"*/15 * * * *":     "2026-10-25 10:30",
		"0 3 * * *":        "2026-10-26 03:00",
		"@daily":           "2026-10-26 00:00",
		"@hourly":          "2026-10-25 11:00",
		"30 9 * * mon-fri": "2026-10-26 09:30",
		"0 0 1 jan *":      "2027-01-01 00:00",
		"0 12 * * 7":       "2026-10-25 12:00",
		"5,20 10 * * *":    "2026-10-25 10:20",
		"0 0 13 * fri":     "2026-10-30 00:00", // day 13 or any Friday
		"10/20 * * * *":    "2026-10-25 10:30",
	} {
		s, err := Parse(spec)
		if err != nil {
			t.Errorf("%s: %v", spec, err)
			continue
		}
		if got := s.Next(from); !got.Equal(at(want)) {
			t.Errorf("%s: next %s, want %s", spec, got.Format("2006-01-02 15:04"), want)
		}
		if !s.Matches(at(want)) {
			t.Errorf("%s doesn't match its own next time", spec)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "5-1 * * * *", "@sometimes", "0 0 30 feb *x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if s, _ := Parse("0 0 30 feb *"); !s.Next(from).IsZero() {
		t.Error("February 30th fired")
	}
}
