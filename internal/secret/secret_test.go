package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	s, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.Seal([]byte("DATABASE_URL=postgres://x"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(box, []byte("postgres")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := s.Open(box)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "DATABASE_URL=postgres://x" {
		t.Fatalf("round trip: %q", got)
	}
	// Tamper detection.
	box[len(box)-1] ^= 0xff
	if _, err := s.Open(box); err == nil {
		t.Fatal("want error on tampered box")
	}
	// Short input.
	if _, err := s.Open([]byte{1, 2}); err == nil {
		t.Fatal("want error on short box")
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("want error for non-32-byte key")
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	s1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	box, err := s1.Seal([]byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	// Second load reads the same key and can open the first sealer's box.
	s2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Open(box); err != nil || string(got) != "v" {
		t.Fatalf("reload open: %q %v", got, err)
	}
	if b, _ := os.ReadFile(path); len(b) != 64 {
		t.Fatalf("key file should be 64 hex chars, got %d", len(b))
	}
}
