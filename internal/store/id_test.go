package store

import (
	"regexp"
	"testing"
)

var idPattern = regexp.MustCompile(`^[a-z0-9]{12}$`)

func TestNewIDShapeAndAlphabet(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := NewID()
		if len(id) != idLength {
			t.Fatalf("len(%q) = %d, want %d", id, len(id), idLength)
		}
		if !idPattern.MatchString(id) {
			t.Fatalf("id %q does not match %s", id, idPattern.String())
		}
	}
}

func TestNewIDUniqueness(t *testing.T) {
	const n = 10000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = true
	}
}
