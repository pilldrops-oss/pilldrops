package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextFindsTheFollowingOccurrence(t *testing.T) {
	for _, tc := range []struct{ expr, from, want string }{
		{"0 9 * * *", "2026-08-02 08:00", "2026-08-02 09:00"},
		{"0 9 * * *", "2026-08-02 09:00", "2026-08-03 09:00"}, // strictly after
		{"0 9 * * *", "2026-08-02 12:00", "2026-08-03 09:00"},
		{"*/15 * * * *", "2026-08-02 09:01", "2026-08-02 09:15"},
		{"*/15 * * * *", "2026-08-02 09:46", "2026-08-02 10:00"},
		{"30 2 1 * *", "2026-08-02 09:00", "2026-09-01 02:30"}, // 1st of the month
		{"0 0 * * 1", "2026-08-02 09:00", "2026-08-03 00:00"},  // next Monday
		{"0 12 * * *", "2026-12-31 13:00", "2027-01-01 12:00"}, // crosses the year
	} {
		got := mustParse(t, tc.expr).Next(at(tc.from))
		if !got.Equal(at(tc.want)) {
			t.Errorf("Parse(%q).Next(%s) = %s, want %s", tc.expr, tc.from, got.Format("2006-01-02 15:04"), tc.want)
		}
	}
}

// 2026 has no Feb 29, so "29 Feb" must skip to 2028 rather than land on Mar 1.
func TestNextHandlesLeapDay(t *testing.T) {
	got := mustParse(t, "0 0 29 2 *").Next(at("2026-03-01 00:00"))
	if want := at("2028-02-29 00:00"); !got.Equal(want) {
		t.Errorf("next Feb 29 = %s, want %s", got.Format("2006-01-02 15:04"), want.Format("2006-01-02 15:04"))
	}
}

// When both day fields are restricted the job runs when either matches.
func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	s := mustParse(t, "0 0 13 * 5") // the 13th, OR any Friday
	// The start date is a Sunday; the next Friday is the 7th, before the 13th.
	if got, want := s.Next(at("2026-08-02 12:00")), at("2026-08-07 00:00"); !got.Equal(want) {
		t.Errorf("dom-or-dow = %s, want %s (a Friday)", got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
	// When only one is restricted it is a plain AND with the wildcard.
	s2 := mustParse(t, "0 0 13 * *")
	if got, want := s2.Next(at("2026-08-02 12:00")), at("2026-08-13 00:00"); !got.Equal(want) {
		t.Errorf("dom-only = %s, want %s", got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
}

func TestShorthands(t *testing.T) {
	for _, tc := range []struct{ expr, from, want string }{
		{"@hourly", "2026-08-02 09:30", "2026-08-02 10:00"},
		{"@daily", "2026-08-02 09:30", "2026-08-03 00:00"},
		{"@midnight", "2026-08-02 09:30", "2026-08-03 00:00"},
		{"@weekly", "2026-08-02 09:30", "2026-08-09 00:00"}, // Sundays
		{"@monthly", "2026-08-02 09:30", "2026-09-01 00:00"},
	} {
		if got := mustParse(t, tc.expr).Next(at(tc.from)); !got.Equal(at(tc.want)) {
			t.Errorf("%s from %s = %s, want %s", tc.expr, tc.from, got.Format("2006-01-02 15:04"), tc.want)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 7",
		"a * * * *", "*/0 * * * *", "5-1 * * * *", "@yearly-ish",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", expr)
		}
	}
}

// "31 February" parses fine and never occurs; it must not hang the sweeper.
func TestImpossibleScheduleReturnsZero(t *testing.T) {
	got := mustParse(t, "0 0 30 2 *").Next(at("2026-08-02 09:00"))
	if !got.IsZero() {
		t.Errorf("Feb 30 returned %s, want the zero time", got)
	}
}

func TestNextIsStrictlyAfterAndMinuteAligned(t *testing.T) {
	s := mustParse(t, "* * * * *")
	from := at("2026-08-02 09:00").Add(30 * time.Second)
	got := s.Next(from)
	if want := at("2026-08-02 09:01"); !got.Equal(want) {
		t.Errorf("Next mid-minute = %s, want %s", got.Format("15:04:05"), want.Format("15:04:05"))
	}
	if got.Second() != 0 || got.Nanosecond() != 0 {
		t.Errorf("Next is not minute-aligned: %s", got)
	}
}
