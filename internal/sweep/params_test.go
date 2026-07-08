package sweep

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

func TestParseParamsDiscrete(t *testing.T) {
	// "y"/"n" are YAML 1.1 boolean literals (go-yaml follows that spec), so
	// the discrete-choice fixture avoids them; the range/grid tests below
	// use plain "x"/"y" via Go literals, which sidesteps YAML entirely.
	space, err := ParseParams([]byte("a: [1, 2]\nb: [p, q]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(space) != 2 {
		t.Fatalf("want 2 params, got %d", len(space))
	}
	if got := space["a"].Choices; len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("a choices = %v", got)
	}
	if got := space["b"].Choices; len(got) != 2 || got[0] != "p" || got[1] != "q" {
		t.Fatalf("b choices = %v", got)
	}
}

func TestParseParamsRange(t *testing.T) {
	space, err := ParseParams([]byte("lr: {min: 1e-5, max: 1e-2, log: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := space["lr"]
	if !ok || len(p.Choices) != 0 {
		t.Fatalf("lr param = %+v", p)
	}
	if p.Min != 1e-5 || p.Max != 1e-2 || !p.Log {
		t.Fatalf("lr range = %+v", p)
	}
}

func TestParseParamsErrors(t *testing.T) {
	cases := map[string][]byte{
		"min>=max":       []byte("lr: {min: 0.1, max: 0.1}\n"),
		"log with min<=0": []byte("lr: {min: 0, max: 1, log: true}\n"),
		"empty list":      []byte("a: []\n"),
		"scalar value":    []byte("a: 5\n"),
		"empty file":      []byte(""),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseParams(b); err == nil {
				t.Fatalf("%s: want error, got nil", name)
			}
		})
	}
}

func TestParseParamsErrorNamesKey(t *testing.T) {
	_, err := ParseParams([]byte("badkey: 5\n"))
	if err == nil {
		t.Fatal("want error")
	}
	if got := err.Error(); got != `param "badkey": want a list or {min,max[,log]}` {
		t.Fatalf("err = %q", got)
	}
}

func TestExpandGrid(t *testing.T) {
	space := map[string]Param{
		"a": {Choices: []string{"1", "2"}},
		"b": {Choices: []string{"x", "y"}},
	}
	sets, truncated, err := Expand(space, 10, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("must not be truncated")
	}
	if len(sets) != 4 {
		t.Fatalf("want 4 sets, got %d: %v", len(sets), sets)
	}
	want := []map[string]string{
		{"a": "1", "b": "x"},
		{"a": "1", "b": "y"},
		{"a": "2", "b": "x"},
		{"a": "2", "b": "y"},
	}
	for i, w := range want {
		if sets[i]["a"] != w["a"] || sets[i]["b"] != w["b"] {
			t.Fatalf("set[%d] = %v, want %v", i, sets[i], w)
		}
	}
}

func TestExpandGridTruncation(t *testing.T) {
	space := map[string]Param{
		"a": {Choices: []string{"1", "2"}},
		"b": {Choices: []string{"x", "y"}},
	}
	sets, truncated, err := Expand(space, 3, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("must be truncated")
	}
	if len(sets) != 3 {
		t.Fatalf("want 3 sets, got %d", len(sets))
	}
	want := []map[string]string{
		{"a": "1", "b": "x"},
		{"a": "1", "b": "y"},
		{"a": "2", "b": "x"},
	}
	for i, w := range want {
		if sets[i]["a"] != w["a"] || sets[i]["b"] != w["b"] {
			t.Fatalf("set[%d] = %v, want %v", i, sets[i], w)
		}
	}
}

func TestExpandRandom(t *testing.T) {
	space := map[string]Param{
		"lr": {Min: 0.0001, Max: 0.1, Log: true},
		"bs": {Choices: []string{"16", "32"}},
	}
	rng := rand.New(rand.NewSource(7))
	sets, truncated, err := Expand(space, 5, rng)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("random expansion is never truncated")
	}
	if len(sets) != 5 {
		t.Fatalf("want 5 sets, got %d", len(sets))
	}
	for _, s := range sets {
		lr, err := strconv.ParseFloat(s["lr"], 64)
		if err != nil {
			t.Fatalf("bad lr value %q: %v", s["lr"], err)
		}
		if lr < 0.0001 || lr > 0.1 {
			t.Fatalf("lr %v out of bounds", lr)
		}
		logLR := math.Log10(lr)
		if logLR < -4 || logLR > -1 {
			t.Fatalf("lr %v not log-uniform (log10=%v)", lr, logLR)
		}
		if s["bs"] != "16" && s["bs"] != "32" {
			t.Fatalf("bs = %q, want 16 or 32", s["bs"])
		}
	}
}
