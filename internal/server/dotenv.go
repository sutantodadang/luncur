package server

import (
	"fmt"
	"strings"
)

// parseDotenv parses .env-style text: one KEY=VALUE per line, blank lines
// and #-comments skipped, an optional leading "export " stripped, value
// unwrapped from one pair of matching single or double quotes. Returns an
// error naming the first malformed line so a bad paste fails whole — no
// partial surprises.
func parseDotenv(text string) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", i+1, line)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out, nil
}
