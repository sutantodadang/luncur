package ai

import (
	"encoding/json"
	"sort"
	"strings"
)

// Redactor scrubs known secret values (app env values, addon passwords, …)
// out of text before it leaves for an LLM provider.
type Redactor struct {
	secrets []string
}

// minSecretLen keeps short, common values ("1", "true", "prod") from turning
// every log line into confetti; they're not meaningful secrets anyway.
const minSecretLen = 6

// NewRedactor builds a Redactor for the given secret values.
func NewRedactor(values ...string) *Redactor {
	seen := map[string]bool{}
	r := &Redactor{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen || seen[v] {
			continue
		}
		seen[v] = true
		r.secrets = append(r.secrets, v)
	}
	// Longest first, so a secret containing another is replaced whole.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	return r
}

// Redact replaces every known secret in s with [REDACTED].
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}

// DecodeJSONObject decodes the first JSON object in text into out. Models
// without structured output sometimes wrap the object in prose or a code
// fence; this tolerates both.
func DecodeJSONObject(text string, out any) error {
	text = strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(text), out); err == nil {
		return nil
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return json.Unmarshal([]byte(text), out) // the original error
	}
	return json.Unmarshal([]byte(text[start:end+1]), out)
}
