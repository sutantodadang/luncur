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
	certProvider, _ := cmd.Flags().GetString("cert-provider")
	if certProvider != "builtin" {
		t.Fatalf("cert-provider default = %q, want builtin", certProvider)
	}
}

func TestUpReplicaFlags(t *testing.T) {
	cmd := upCmd()
	for _, name := range []string{"replica-url", "replica-endpoint", "replica-access-key", "replica-secret-key"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing flag --%s", name)
		}
	}
}

func TestUpReplicaURLRequiresCredentials(t *testing.T) {
	cmd := upCmd()
	cmd.SetArgs([]string{"--kubeconfig", "does-not-exist", "--replica-url", "s3://bucket/luncur"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--replica-access-key") {
		t.Fatalf("want replica credentials error, got %v", err)
	}
}

func TestUpReplicaCredentialsRequireURL(t *testing.T) {
	cmd := upCmd()
	cmd.SetArgs([]string{"--kubeconfig", "does-not-exist", "--replica-access-key", "AKIA", "--replica-secret-key", "secret"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--replica-url") {
		t.Fatalf("want replica-url required error, got %v", err)
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
