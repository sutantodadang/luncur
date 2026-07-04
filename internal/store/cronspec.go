package store

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// cronFieldBounds are the valid numeric ranges for the 5 standard cron
// fields, in order: minute, hour, day-of-month, month, day-of-week (0 and 7
// both mean Sunday).
var cronFieldBounds = [5][2]int{
	{0, 59},
	{0, 23},
	{1, 31},
	{1, 12},
	{0, 7},
}

// cronTokenRe matches one comma-separated token: `*` | `*/n` | `n` | `n-m` |
// `n-m/s`.
var cronTokenRe = regexp.MustCompile(`^(\*|\d+)(-\d+)?(/\d+)?$`)

// validCronSpec validates s as a standard 5-field cron expression (minute
// hour day-of-month month day-of-week). It only checks syntax and numeric
// bounds — Kubernetes' CronJob controller is what actually evaluates the
// schedule at runtime.
func validCronSpec(s string) error {
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return fmt.Errorf("cron schedule must have exactly 5 fields, got %d", len(fields))
	}
	for i, field := range fields {
		lo, hi := cronFieldBounds[i][0], cronFieldBounds[i][1]
		for _, token := range strings.Split(field, ",") {
			if err := validCronToken(token, lo, hi); err != nil {
				return fmt.Errorf("field %d (%q): %w", i+1, field, err)
			}
		}
	}
	return nil
}

func validCronToken(token string, lo, hi int) error {
	m := cronTokenRe.FindStringSubmatch(token)
	if m == nil {
		return fmt.Errorf("invalid token %q", token)
	}
	base, rangeEnd, step := m[1], m[2], m[3]

	if base == "*" {
		if rangeEnd != "" {
			return fmt.Errorf("invalid token %q: %s cannot have a range", token, base)
		}
	} else {
		n, _ := strconv.Atoi(base)
		if n < lo || n > hi {
			return fmt.Errorf("value %d out of range %d-%d", n, lo, hi)
		}
		if rangeEnd != "" {
			end, _ := strconv.Atoi(strings.TrimPrefix(rangeEnd, "-"))
			if end < lo || end > hi {
				return fmt.Errorf("value %d out of range %d-%d", end, lo, hi)
			}
			if end < n {
				return fmt.Errorf("range %q is backwards", token)
			}
		}
	}
	if step != "" {
		s, _ := strconv.Atoi(strings.TrimPrefix(step, "/"))
		if s <= 0 {
			return fmt.Errorf("step %q must be positive", token)
		}
	}
	return nil
}
