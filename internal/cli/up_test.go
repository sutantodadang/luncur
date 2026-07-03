package cli

import (
	"runtime"
	"strings"
	"testing"
)

func TestUpDefaults(t *testing.T) {
	cmd := upCmd()
	if cmd.Use != "up" {
		t.Fatal("use")
	}
	img, _ := cmd.Flags().GetString("image")
	if img != "ghcr.io/sutantodadang/luncur:latest" { // version == "dev" in tests
		t.Fatalf("image default = %q", img)
	}
}

func TestUpRefusesNonLinuxWithoutKubeconfig(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux host")
	}
	cmd := upCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil ||
		!strings.Contains(err.Error(), "linux") {
		t.Fatalf("want linux-only error, got %v", err)
	}
}
