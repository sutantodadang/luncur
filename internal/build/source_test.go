package build

import (
	"strings"
	"testing"
)

func TestSourceSaveAndRead(t *testing.T) {
	s, err := NewSource(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Save("42", strings.NewReader("tarbytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.TarballPath("42"); got != path {
		t.Fatalf("TarballPath=%q want %q", got, path)
	}
	// No log written yet → ReadLog returns (nil, nil), not an error.
	log, err := s.ReadLog("42")
	if err != nil || log != nil {
		t.Fatalf("ReadLog on missing = (%q, %v), want (nil, nil)", log, err)
	}
}
