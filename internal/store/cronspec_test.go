package store

import "testing"

func TestValidCronSpec(t *testing.T) {
	valid := []string{
		"* * * * *",
		"*/5 0-8 1,15 * 1-5",
		"0 3 * * *",
	}
	for _, s := range valid {
		if err := validCronSpec(s); err != nil {
			t.Errorf("validCronSpec(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"* * * *",       // 4 fields
		"60 * * * *",    // minute out of range
		"* * * * 8",     // weekday out of range
		"a * * * *",     // non-numeric token
		"",               // empty
		"* * * *  ",      // still 4 fields once whitespace collapses
	}
	for _, s := range invalid {
		if err := validCronSpec(s); err == nil {
			t.Errorf("validCronSpec(%q) = nil, want error", s)
		}
	}
}
