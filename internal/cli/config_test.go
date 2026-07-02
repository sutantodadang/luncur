package cli

import (
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	want := Config{Server: "http://x:8080", Token: "lcr_abc"}
	if err := saveConfig(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("want %+v, got %+v", want, got)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "nope.json"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("want error for missing config")
	}
}
