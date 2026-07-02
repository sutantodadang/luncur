package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("want version output containing 'dev', got %q", out.String())
	}
}
