package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseWhen resolves a spoken time expression relative to now, in now's
// location. It is deliberately strict: any word it doesn't understand is an
// error, so a reminder is never silently scheduled at the wrong time.
//
// Supported: "in 20 minutes", "in an hour", "in half an hour", "in 2 days",
// "tomorrow", "tonight", "today at 5pm", "tomorrow morning", "friday at 3",
// "next monday 9:30am", "noon", "at 17:45", "the day after tomorrow",
// "next week", and ISO forms like "2026-11-10 09:00" or RFC 3339.
func ParseWhen(s string, now time.Time) (time.Time, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, raw, now.Location()); err == nil {
			if layout == "2006-01-02" {
				t = t.Add(9 * time.Hour)
			}
			return t, nil
		}
	}

	toks := tokenize(raw)
	if len(toks) > 1 && toks[0] == "in" {
		return parseRelative(toks[1:], now, raw)
	}

	var (
		day     *time.Time // midnight of the chosen day
		hour    = -1
		min     = 0
		defHour = 9 // used when only a day is given
		vague   = false
	)
	setDay := func(d time.Time) error {
		if day != nil {
			return fmt.Errorf("two different days in %q", raw)
		}
		day = &d
		return nil
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case filler[t]:
		case t == "today":
			if err := setDay(midnight); err != nil {
				return time.Time{}, err
			}
		case t == "tonight":
			if err := setDay(midnight); err != nil {
				return time.Time{}, err
			}
			defHour, vague = 20, true
		case t == "tomorrow":
			if err := setDay(midnight.AddDate(0, 0, 1)); err != nil {
				return time.Time{}, err
			}
		case t == "day" && i+2 < len(toks) && toks[i+1] == "after" && toks[i+2] == "tomorrow":
			if err := setDay(midnight.AddDate(0, 0, 2)); err != nil {
				return time.Time{}, err
			}
			i += 2
		case t == "next" && i+1 < len(toks) && toks[i+1] == "week":
			// Next Monday.
			d := int(time.Monday-midnight.Weekday()+7) % 7
			if d == 0 {
				d = 7
			}
			if err := setDay(midnight.AddDate(0, 0, d)); err != nil {
				return time.Time{}, err
			}
			i++
		case t == "next" || t == "this" || t == "coming":
			// Modifier for a following weekday; "next friday" = the coming one.
		case weekdays[t] != nil:
			wd := *weekdays[t]
			d := (int(wd) - int(midnight.Weekday()) + 7) % 7
			if d == 0 {
				d = 7 // "monday" said on a Monday means next week
			}
			if err := setDay(midnight.AddDate(0, 0, d)); err != nil {
				return time.Time{}, err
			}
		case parts[t] != 0:
			defHour, vague = parts[t], true
		case t == "noon" || t == "midday":
			hour, min = 12, 0
		case t == "midnight":
			hour, min = 0, 0
			if day == nil {
				d := midnight.AddDate(0, 0, 1)
				day = &d
			}
		default:
			h, m, ok := clock(t, toks, &i)
			if !ok {
				return time.Time{}, fmt.Errorf("couldn't understand %q in %q; try e.g. \"tomorrow at 9am\" or \"in 2 hours\"", t, raw)
			}
			if hour >= 0 {
				return time.Time{}, fmt.Errorf("two different times in %q", raw)
			}
			hour, min = h, m
		}
	}

	if hour < 0 && day == nil && !vague {
		return time.Time{}, fmt.Errorf("no time found in %q", raw)
	}
	// "morning", "evening", etc. disambiguate a bare hour: "7 in the evening".
	if hour >= 0 && hour < 12 && vague && defHour >= 15 {
		hour += 12
	}
	if hour < 0 {
		hour = defHour
	}
	if day == nil {
		// Time only: the next time the clock shows it.
		t := midnight.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	t := day.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("%q is in the past (now %s)", raw, now.Format("Mon 3:04 PM"))
	}
	return t, nil
}

var filler = map[string]bool{"at": true, "on": true, "by": true, "around": true, "the": true, "in": true, "o'clock": true, "oclock": true, "of": true}

var parts = map[string]int{"morning": 9, "afternoon": 15, "evening": 18, "night": 20}

var weekdays = func() map[string]*time.Weekday {
	m := map[string]*time.Weekday{}
	for d := time.Sunday; d <= time.Saturday; d++ {
		wd := d
		name := strings.ToLower(d.String())
		m[name] = &wd
		m[name[:3]] = &wd
	}
	m["tues"], m["thur"], m["thurs"] = m["tue"], m["thu"], m["thu"]
	return m
}()

var clockRe = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm|a\.m\.|p\.m\.)?$`)

// clock parses "9", "9am", "9:30", "21:00", optionally followed by a separate
// "am"/"pm" token.
func clock(t string, toks []string, i *int) (h, m int, ok bool) {
	g := clockRe.FindStringSubmatch(t)
	if g == nil {
		return 0, 0, false
	}
	h, _ = strconv.Atoi(g[1])
	if g[2] != "" {
		m, _ = strconv.Atoi(g[2])
	}
	suffix := g[3]
	if suffix == "" && *i+1 < len(toks) && (toks[*i+1] == "am" || toks[*i+1] == "pm") {
		suffix = toks[*i+1]
		*i++
	}
	if h > 23 || m > 59 {
		return 0, 0, false
	}
	switch strings.ReplaceAll(suffix, ".", "") {
	case "am":
		if h > 12 {
			return 0, 0, false
		}
		if h == 12 {
			h = 0
		}
	case "pm":
		if h > 12 {
			return 0, 0, false
		}
		if h < 12 {
			h += 12
		}
	default:
		// Bare "at 3" in conversation means 3 PM; "at 8" means 8 AM.
		if h >= 1 && h <= 6 && g[2] == "" {
			h += 12
		}
	}
	return h, m, true
}

var numWords = map[string]int{"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "fifteen": 15, "twenty": 20, "thirty": 30, "forty-five": 45, "few": 3, "couple": 2}

func parseRelative(toks []string, now time.Time, raw string) (time.Time, error) {
	// "half an hour"
	if len(toks) >= 3 && toks[0] == "half" && (toks[1] == "an" || toks[1] == "a") && strings.HasPrefix(toks[2], "hour") && len(toks) == 3 {
		return now.Add(30 * time.Minute).Truncate(time.Minute), nil
	}
	var total time.Duration
	for i := 0; i < len(toks); i++ {
		if toks[i] == "and" || toks[i] == "a" && i+1 < len(toks) && toks[i+1] == "couple" {
			continue
		}
		n, err := strconv.ParseFloat(toks[i], 64)
		if err != nil {
			v, ok := numWords[toks[i]]
			if !ok {
				return time.Time{}, fmt.Errorf("couldn't understand %q in %q", toks[i], raw)
			}
			n = float64(v)
		}
		if i+1 < len(toks) && toks[i+1] == "of" {
			i++ // "a couple of hours"
		}
		if i+1 >= len(toks) {
			return time.Time{}, fmt.Errorf("missing unit in %q", raw)
		}
		i++
		var unit time.Duration
		switch strings.TrimSuffix(toks[i], "s") {
		case "sec", "second":
			unit = time.Second
		case "min", "minute":
			unit = time.Minute
		case "hr", "hour":
			unit = time.Hour
		case "day":
			unit = 24 * time.Hour
		case "week":
			unit = 7 * 24 * time.Hour
		default:
			return time.Time{}, fmt.Errorf("unknown unit %q in %q", toks[i], raw)
		}
		total += time.Duration(n * float64(unit))
	}
	if total <= 0 {
		return time.Time{}, fmt.Errorf("no duration in %q", raw)
	}
	return now.Add(total), nil
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(",", " ", "!", " ", "?", " ", ";", " ").Replace(s)
	// Normalize "a.m."/"p.m." (and "9a.m.") before trimming sentence dots.
	s = strings.NewReplacer("a.m.", "am", "p.m.", "pm", "a.m", "am", "p.m", "pm").Replace(s)
	s = strings.TrimRight(strings.TrimSpace(s), ".")
	return strings.Fields(s)
}
