package tools

import (
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("PST", -8*3600)
	now := time.Date(2026, 11, 8, 15, 10, 0, 0, loc) // Sunday 3:10 PM
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc) }
	cases := map[string]time.Time{
		"tomorrow":                  at(9, 9, 0),
		"tomorrow at 9am":           at(9, 9, 0),
		"Tomorrow at 9 a.m.":        at(9, 9, 0),
		"tomorrow 9 pm":             at(9, 21, 0),
		"tomorrow morning":          at(9, 9, 0),
		"tomorrow evening":          at(9, 18, 0),
		"tomorrow evening at 7":     at(9, 19, 0),
		"tomorrow at 3":             at(9, 15, 0),
		"tomorrow at 10:30":         at(9, 10, 30),
		"tonight":                   at(8, 20, 0),
		"today at 5pm":              at(8, 17, 0),
		"5pm":                       at(8, 17, 0),
		"at 2pm":                    at(9, 14, 0), // already past today
		"noon":                      at(9, 12, 0),
		"17:45":                     at(8, 17, 45),
		"monday":                    at(9, 9, 0),
		"next friday at 3pm":        at(13, 15, 0),
		"sunday":                    at(15, 9, 0), // said on Sunday → next week
		"the day after tomorrow":    at(10, 9, 0),
		"next week":                 at(9, 9, 0),
		"in 20 minutes":             now.Add(20 * time.Minute),
		"in an hour":                now.Add(time.Hour),
		"in half an hour":           now.Add(30 * time.Minute),
		"in 2 hours and 30 minutes": now.Add(150 * time.Minute),
		"in a couple of days":       now.Add(48 * time.Hour),
		"2026-11-10 09:00":          at(10, 9, 0),
		"2026-11-10":                at(10, 9, 0),
		"2026-11-10T08:15:00-08:00": at(10, 8, 15),
	}
	for in, want := range cases {
		got, err := ParseWhen(in, now)
		if err != nil {
			t.Errorf("%q: %v", in, err)
		} else if !got.Equal(want) {
			t.Errorf("%q = %s, want %s", in, got.Format(time.RFC1123), want.Format(time.RFC1123))
		}
	}
	for _, bad := range []string{"", "whenever", "tomorrow-ish", "today at 9am", "tomorrow at 25", "in 3 fortnights", "monday tuesday", "13pm"} {
		if got, err := ParseWhen(bad, now); err == nil {
			t.Errorf("%q should fail, got %s", bad, got)
		}
	}
}
