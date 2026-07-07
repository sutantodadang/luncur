package cli

import (
	"strings"
	"testing"
)

func TestProjectGPUQuotaCmd(t *testing.T) {
	srv := testEnv(t)

	// Login first
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}

	// Create a project
	if _, err := run(t, "project", "create", "p1"); err != nil {
		t.Fatal(err)
	}

	// Set GPU quota
	out, err := run(t, "project", "gpu-quota", "p1", "3")
	if err != nil {
		t.Fatalf("gpu-quota: %v (%s)", err, out)
	}
	if !strings.Contains(out, "3") {
		t.Fatalf("out = %s", out)
	}
}
