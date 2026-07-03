package store

import (
	"errors"
	"testing"
)

func TestSettings(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetSetting("cert_provider"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset key: %v, want ErrNotFound", err)
	}
	if err := s.SetSetting("cert_provider", "builtin"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("cert_provider", "traefik"); err != nil {
		t.Fatal(err) // upsert
	}
	v, err := s.GetSetting("cert_provider")
	if err != nil || v != "traefik" {
		t.Fatalf("got %q err=%v, want traefik", v, err)
	}
}
