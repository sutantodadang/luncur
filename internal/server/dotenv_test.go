package server

import (
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "simple pair",
			in:   "KEY=value",
			want: map[string]string{"KEY": "value"},
		},
		{
			name: "value containing equals splits on first only",
			in:   "URL=postgres://x?a=1",
			want: map[string]string{"URL": "postgres://x?a=1"},
		},
		{
			name: "blank lines and comments skipped",
			in:   "A=1\n\n# a comment\nB=2",
			want: map[string]string{"A": "1", "B": "2"},
		},
		{
			name: "export prefix stripped",
			in:   "export KEY=value",
			want: map[string]string{"KEY": "value"},
		},
		{
			name: "double quoted value unwrapped",
			in:   `KEY="value"`,
			want: map[string]string{"KEY": "value"},
		},
		{
			name: "single quoted value unwrapped",
			in:   `KEY='value'`,
			want: map[string]string{"KEY": "value"},
		},
		{
			name: "quoted value keeps inner spaces",
			in:   `KEY="two words"`,
			want: map[string]string{"KEY": "two words"},
		},
		{
			name: "unmatched quote left as-is",
			in:   `KEY="oops`,
			want: map[string]string{"KEY": `"oops`},
		},
		{
			name: "crlf line endings",
			in:   "A=1\r\nB=2\r\n",
			want: map[string]string{"A": "1", "B": "2"},
		},
		{
			name: "empty input yields empty map",
			in:   "",
			want: map[string]string{},
		},
		{
			name:    "malformed line missing equals",
			in:      "NOVALUE",
			wantErr: true,
		},
		{
			name:    "empty key",
			in:      "=x",
			wantErr: true,
		},
		{
			name: "later duplicate key wins",
			in:   "A=1\nA=2",
			want: map[string]string{"A": "2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDotenv(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDotenv(%q): want error, got none", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDotenv(%q): unexpected error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseDotenv(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("parseDotenv(%q)[%q] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}

func TestParseDotenvMalformedLineNumber(t *testing.T) {
	_, err := parseDotenv("A=1\nNOVALUE\nB=2")
	if err == nil {
		t.Fatal("want error, got none")
	}
	if got := err.Error(); !strings.Contains(got, "line 2") {
		t.Fatalf("error %q: want to mention line 2", got)
	}
}
