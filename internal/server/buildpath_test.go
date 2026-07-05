package server

import (
	"strings"
	"testing"
)

func TestValidBuildPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"dashboard", "dashboard", false},
		{"apps/backend", "apps/backend", false},
		{"", "", false},
		{"  dashboard  ", "dashboard", false},
		{"dashboard/", "dashboard", false},
		{"../x", "", true},
		{"/abs", "", true},
		{`a\b`, "", true},
		{"a/../b", "", true},
		{"./dashboard", "", true},
		{"C:\\foo", "", true},
		{strings.Repeat("a", 201), "", true},
	}
	for _, c := range cases {
		got, err := validBuildPath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("validBuildPath(%q): want error, got nil (result %q)", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("validBuildPath(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("validBuildPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
