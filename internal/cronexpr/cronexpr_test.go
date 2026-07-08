package cronexpr

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

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func TestMatchesEveryFifteenMinutes(t *testing.T) {
	s := mustParse(t, "*/15 * * * *")
	for _, mm := range []int{0, 15, 30, 45} {
		tm := utc(2026, 7, 8, 10, mm)
		if !s.Matches(tm) {
			t.Errorf("Matches(%v) = false, want true", tm)
		}
	}
	for _, mm := range []int{1, 14, 16, 29, 31, 44, 46, 59} {
		tm := utc(2026, 7, 8, 10, mm)
		if s.Matches(tm) {
			t.Errorf("Matches(%v) = true, want false", tm)
		}
	}
}

func TestMatchesDailyAtThreeAM(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	if !s.Matches(utc(2026, 7, 8, 3, 0)) {
		t.Fatal("want match at 03:00")
	}
	if s.Matches(utc(2026, 7, 8, 3, 1)) {
		t.Fatal("must not match at 03:01")
	}
	if s.Matches(utc(2026, 7, 8, 4, 0)) {
		t.Fatal("must not match at 04:00")
	}
}

func TestMatchesFirstOfMonth(t *testing.T) {
	s := mustParse(t, "30 6 1 * *")
	if !s.Matches(utc(2026, 8, 1, 6, 30)) {
		t.Fatal("want match on the 1st at 06:30")
	}
	if s.Matches(utc(2026, 8, 2, 6, 30)) {
		t.Fatal("must not match on the 2nd")
	}
}

// Standard cron dom/dow OR rule: when BOTH dom and dow are restricted, a time
// matches if EITHER matches. "0 0 13 * 5" fires on Friday the 6th (dow match)
// AND on the 13th of any weekday (dom match).
func TestDomDowOrRule(t *testing.T) {
	s := mustParse(t, "0 0 13 * 5")
	// 2026-07-13 is a Monday (dom matches, dow doesn't).
	if !s.Matches(utc(2026, 7, 13, 0, 0)) {
		t.Fatal("want match on the 13th (dom matches)")
	}
	// The nearest Friday to July 13, 2026: 2026-07-10 is a Friday (dow
	// matches, dom doesn't).
	fri := utc(2026, 7, 10, 0, 0)
	if fri.Weekday() != time.Friday {
		t.Fatalf("test fixture bug: %v is not a Friday", fri)
	}
	if !s.Matches(fri) {
		t.Fatal("want match on a Friday (dow matches)")
	}
	// A Tuesday that isn't the 13th: neither matches.
	tue := utc(2026, 7, 14, 0, 0)
	if tue.Weekday() != time.Tuesday {
		t.Fatalf("test fixture bug: %v is not a Tuesday", tue)
	}
	if s.Matches(tue) {
		t.Fatal("must not match: neither dom nor dow matches")
	}
}

func TestDowSevenIsSunday(t *testing.T) {
	s := mustParse(t, "0 0 * * 7")
	sun := utc(2026, 7, 12, 0, 0)
	if sun.Weekday() != time.Sunday {
		t.Fatalf("test fixture bug: %v is not a Sunday", sun)
	}
	if !s.Matches(sun) {
		t.Fatal("dow 7 must mean Sunday")
	}
	mon := utc(2026, 7, 13, 0, 0)
	if s.Matches(mon) {
		t.Fatal("must not match Monday")
	}
}

func TestMatchesTruncatesToMinute(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	tm := time.Date(2026, 7, 8, 3, 0, 45, 999, time.UTC)
	if !s.Matches(tm) {
		t.Fatal("want match: seconds/nanos must be truncated away")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []string{
		"* * * * * *",  // 6 fields
		"* * *",        // too few fields
		"60 * * * *",   // minute out of range
		"* * 0 * *",    // dom out of range (min 1)
		"*/0 * * * *",  // zero step
		"5-1 * * * *",  // reversed range
		"garbage * * * *",
		"* 24 * * *",  // hour out of range
		"* * * 13 *",  // month out of range
		"* * * * 8",   // dow out of range (max 7)
		"",
	}
	for _, expr := range cases {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q): want error, got nil", expr)
		}
	}
}

func TestParseValidSyntaxVariants(t *testing.T) {
	cases := []string{
		"* * * * *",
		"1,15,30 * * * *",
		"1-5 * * * *",
		"*/15 * * * *",
		"10-30/5 * * * *",
		"0 0 * * 0",
	}
	for _, expr := range cases {
		if _, err := Parse(expr); err != nil {
			t.Errorf("Parse(%q): unexpected error: %v", expr, err)
		}
	}
}
