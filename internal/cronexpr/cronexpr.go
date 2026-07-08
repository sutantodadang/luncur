// Package cronexpr implements a minimal 5-field cron expression parser and
// matcher (minute hour day-of-month month day-of-week), stdlib only. Numbers
// only — no names (jan/mon), no @daily-style macros.
package cronexpr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression (minute hour day-of-month
// month day-of-week). Supported syntax: "*", single numbers, lists "1,15",
// ranges "1-5", steps "*/15" and "10-30/5". Day-of-week 0-6, 0=Sunday
// (7 also accepted as Sunday, normalized to 0). No names (jan/mon), no
// @daily macros — numbers only.
type Schedule struct {
	Minute, Hour, Dom, Month, Dow map[int]bool
}

// fieldSpec describes one of the 5 cron fields for parsing/validation.
type fieldSpec struct {
	name     string
	min, max int
}

var fieldSpecs = [5]fieldSpec{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"dom", 1, 31},
	{"month", 1, 12},
	{"dow", 0, 7},
}

// Parse parses a 5-field cron expression. The error names the offending
// field.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cronexpr: want 5 fields (minute hour dom month dow), got %d in %q", len(fields), expr)
	}

	sets := make([]map[int]bool, 5)
	for i, spec := range fieldSpecs {
		set, err := parseField(fields[i], spec)
		if err != nil {
			return Schedule{}, fmt.Errorf("cronexpr: field %s: %w", spec.name, err)
		}
		sets[i] = set
	}

	// Normalize dow 7 -> 0 (Sunday).
	dow := sets[4]
	if dow[7] {
		delete(dow, 7)
		dow[0] = true
	}

	return Schedule{
		Minute: sets[0],
		Hour:   sets[1],
		Dom:    sets[2],
		Month:  sets[3],
		Dow:    sets[4],
	}, nil
}

// parseField parses one comma-separated field into the set of integers it
// selects, validating every value against spec's [min,max] range.
func parseField(field string, spec fieldSpec) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty term")
		}
		if err := parseTerm(part, spec, set); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// parseTerm parses one comma-delimited term — "*", "*/N", "N", "N-M", or
// "N-M/S" — adding every selected value into set.
func parseTerm(term string, spec fieldSpec, set map[int]bool) error {
	rangePart := term
	step := 1
	if idx := strings.IndexByte(term, '/'); idx >= 0 {
		rangePart = term[:idx]
		s, err := strconv.Atoi(term[idx+1:])
		if err != nil {
			return fmt.Errorf("bad step in %q: %w", term, err)
		}
		if s <= 0 {
			return fmt.Errorf("step must be positive in %q", term)
		}
		step = s
	}

	var lo, hi int
	switch {
	case rangePart == "*":
		lo, hi = spec.min, spec.max
	case strings.Contains(rangePart, "-"):
		bounds := strings.SplitN(rangePart, "-", 2)
		if len(bounds) != 2 {
			return fmt.Errorf("bad range %q", rangePart)
		}
		var err error
		lo, err = strconv.Atoi(bounds[0])
		if err != nil {
			return fmt.Errorf("bad range start in %q: %w", rangePart, err)
		}
		hi, err = strconv.Atoi(bounds[1])
		if err != nil {
			return fmt.Errorf("bad range end in %q: %w", rangePart, err)
		}
		if lo > hi {
			return fmt.Errorf("reversed range %q", rangePart)
		}
	default:
		v, err := strconv.Atoi(rangePart)
		if err != nil {
			return fmt.Errorf("bad value %q", rangePart)
		}
		lo, hi = v, v
	}

	if lo < spec.min || hi > spec.max {
		return fmt.Errorf("value out of range [%d,%d]: %q", spec.min, spec.max, term)
	}

	for v := lo; v <= hi; v += step {
		set[v] = true
	}
	return nil
}

// Matches reports whether t (truncated to the minute, evaluated in UTC)
// satisfies the schedule. Standard cron dom/dow rule: when BOTH dom and
// dow are restricted (neither is "*"), a time matches if EITHER matches.
func (s Schedule) Matches(t time.Time) bool {
	t = t.UTC().Truncate(time.Minute)

	if !s.Minute[t.Minute()] || !s.Hour[t.Hour()] || !s.Month[int(t.Month())] {
		return false
	}

	domRestricted := len(s.Dom) < 31
	dowRestricted := len(s.Dow) < 7
	domMatch := s.Dom[t.Day()]
	dowMatch := s.Dow[int(t.Weekday())]

	switch {
	case domRestricted && dowRestricted:
		return domMatch || dowMatch
	default:
		return domMatch && dowMatch
	}
}
