package cli

import "testing"

func TestParseForwardPorts(t *testing.T) {
	cases := []struct {
		arg           string
		appPort       int
		local, remote int
		wantErr       bool
	}{
		{"", 3000, 3000, 3000, false},
		{"8080", 3000, 8080, 3000, false},
		{"8080:80", 3000, 8080, 80, false},
		{"0:80", 3000, 0, 0, true},
		{"x", 3000, 0, 0, true},
		{"8080:y", 3000, 0, 0, true},
	}
	for _, c := range cases {
		l, r, err := parseForwardPorts(c.arg, c.appPort)
		if c.wantErr != (err != nil) {
			t.Fatalf("%q: err=%v", c.arg, err)
		}
		if err == nil && (l != c.local || r != c.remote) {
			t.Fatalf("%q: got %d:%d want %d:%d", c.arg, l, r, c.local, c.remote)
		}
	}
}

func TestForwardArgSplit(t *testing.T) {
	if _, _, err := splitProjectApp("webonly"); err == nil {
		t.Fatal("want error for missing slash")
	}
	p, a, err := splitProjectApp("waku/waku-simpaniz")
	if err != nil || p != "waku" || a != "waku-simpaniz" {
		t.Fatalf("got %s %s %v", p, a, err)
	}
}
