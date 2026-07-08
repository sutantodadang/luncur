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

// TestProjectQuotaCmd mirrors TestProjectGPUQuotaCmd for D4's CPU/memory
// quota: set both flags, clear via --off, and reject a bare call with
// neither flag nor --off.
func TestProjectQuotaCmd(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p1"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "project", "quota", "p1", "--cpu", "4000", "--memory", "8192")
	if err != nil {
		t.Fatalf("quota: %v (%s)", err, out)
	}
	if !strings.Contains(out, "4000") || !strings.Contains(out, "8192") {
		t.Fatalf("out = %s", out)
	}

	out, err = run(t, "project", "quota", "p1", "--off")
	if err != nil {
		t.Fatalf("quota --off: %v (%s)", err, out)
	}
	if !strings.Contains(out, "cleared") {
		t.Fatalf("out = %s", out)
	}

	if _, err := run(t, "project", "quota", "p1"); err == nil {
		t.Fatal("want error when neither flag nor --off is set")
	}
}
