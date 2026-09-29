// Package cron parses 5-field cron expressions and answers when a schedule
// next fires. The parser is strict: a typo in a schedule is a job that silently never runs.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// bound is one field's valid range.
type bound struct {
	min, max int
	name     string
}

var bounds = [5]bound{
	{0, 59, "minute"},
	{0, 23, "hour"},
	{1, 31, "day-of-month"},
	{1, 12, "month"},
	{0, 6, "day-of-week"},
}

// Schedule is a parsed expression. domRestricted/dowRestricted record whether
// the field was a bare "*", which decides between AND and OR in dayMatches.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domRestricted, dowRestricted  bool
}

var shorthands = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

// Parse reads a 5-field expression ("minute hour day-of-month month
// day-of-week") or one of the @shorthands.
func Parse(expr string) (Schedule, error) {
	e := strings.TrimSpace(expr)
	if strings.HasPrefix(e, "@") {
		sub, ok := shorthands[strings.ToLower(e)]
		if !ok {
			return Schedule{}, fmt.Errorf("unknown shorthand %q", e)
		}
		e = sub
	}
	f := strings.Fields(e)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("expected 5 fields, got %d in %q", len(f), expr)
	}
	var s Schedule
	sets := [5]*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	for i, raw := range f {
		set, err := parseField(raw, bounds[i])
		if err != nil {
			return Schedule{}, err
		}
		*sets[i] = set
	}
	s.domRestricted = f[2] != "*"
	s.dowRestricted = f[4] != "*"
	return s, nil
}

// parseField turns one field into a bitset. Handles "*", "a", "a-b", "*/n",
// "a-b/n" and comma-separated lists of those.
func parseField(raw string, b bound) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(raw, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty %s entry in %q", b.name, raw)
		}
		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad %s step %q", b.name, stepStr)
			}
			step, part = n, base
		}
		lo, hi := b.min, b.max
		if part != "*" {
			var err error
			if loStr, hiStr, ok := strings.Cut(part, "-"); ok {
				if lo, err = strconv.Atoi(loStr); err != nil {
					return 0, fmt.Errorf("bad %s %q", b.name, loStr)
				}
				if hi, err = strconv.Atoi(hiStr); err != nil {
					return 0, fmt.Errorf("bad %s %q", b.name, hiStr)
				}
				if lo > hi {
					return 0, fmt.Errorf("%s range %q is backwards", b.name, part)
				}
			} else {
				if lo, err = strconv.Atoi(part); err != nil {
					return 0, fmt.Errorf("bad %s %q", b.name, part)
				}
				hi = lo
			}
		}
		if lo < b.min || hi > b.max {
			return 0, fmt.Errorf("%s %d-%d out of range %d-%d", b.name, lo, hi, b.min, b.max)
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	if set == 0 {
		return 0, fmt.Errorf("%s matches nothing in %q", b.name, raw)
	}
	return set, nil
}

func has(set uint64, v int) bool { return set&(1<<uint(v)) != 0 }

// Location resolves an IANA zone name, falling back to UTC, never time.Local:
// local time is the server's accident.
func Location(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ValidTimezone reports whether a zone name resolves, so the API can reject a typo at write time.
func ValidTimezone(name string) bool {
	if name == "" {
		return true // means UTC
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// maxLookahead bounds the search; five years covers the Feb 29 cycle. An
// unreachable schedule ("31 February") returns the zero time instead of spinning.
const maxLookahead = 5 * 366 * 24 * time.Hour

// Next returns the first firing time strictly after t, truncated to the minute,
// or the zero time if the schedule is unreachable. Strictly after, or the
// sweeper would fire in a loop.
func (s Schedule) Next(t time.Time) time.Time {
	// Step one minute forward so t itself never matches.
	cur := t.Truncate(time.Minute).Add(time.Minute)
	deadline := t.Add(maxLookahead)
	for cur.Before(deadline) {
		// Skip whole days that cannot match, or a yearly schedule costs half a million comparisons.
		if !s.dayMatches(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, cur.Location()).AddDate(0, 0, 1)
			continue
		}
		if has(s.hour, cur.Hour()) && has(s.minute, cur.Minute()) {
			return cur
		}
		cur = cur.Add(time.Minute)
	}
	return time.Time{}
}

// dayMatches applies cron's day rule: when both day-of-month and day-of-week
// are restricted the day matches if either does. "0 0 13 * 5" means the 13th or any Friday.
func (s Schedule) dayMatches(t time.Time) bool {
	if !has(s.month, int(t.Month())) {
		return false
	}
	dom, dow := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	if s.domRestricted && s.dowRestricted {
		return dom || dow
	}
	return dom && dow
}
