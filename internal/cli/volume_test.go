package cli

import (
	"strings"
	"testing"
)

func TestVolumeCommands(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "web", "--project", "proj", "--port", "8080"); err != nil {
		t.Fatal(err)
	}

	// --size is required.
	if _, err := run(t, "volume", "add", "web", "/data", "--project", "proj"); err == nil {
		t.Fatal("volume add without --size should fail")
	}

	// Add with defaulted name, prints the created line + the server warning.
	out, err := run(t, "volume", "add", "web", "/var/lib/data", "--project", "proj", "--size", "5")
	if err != nil {
		t.Fatalf("volume add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "created data (5GB at /var/lib/data)") {
		t.Fatalf("want created line, got %q", out)
	}
	if !strings.Contains(out, "Recreate") || !strings.Contains(out, "backup") {
		t.Fatalf("want server warning in output, got %q", out)
	}

	// Add with explicit --name.
	out, err = run(t, "volume", "add", "web", "/var/cache", "--project", "proj", "--size", "1", "--name", "cache")
	if err != nil {
		t.Fatalf("volume add named: %v (%s)", err, out)
	}
	if !strings.Contains(out, "created cache (1GB at /var/cache)") {
		t.Fatalf("want named created line, got %q", out)
	}

	// List shows NAME PATH SIZE columns.
	out, err = run(t, "volume", "list", "web", "--project", "proj")
	if err != nil {
		t.Fatalf("volume list: %v (%s)", err, out)
	}
	for _, want := range []string{"NAME", "PATH", "SIZE", "data", "/var/lib/data", "5GB", "cache", "/var/cache", "1GB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in list, got %q", want, out)
		}
	}

	// Remove without purge keeps the PVC.
	out, err = run(t, "volume", "remove", "web", "cache", "--project", "proj")
	if err != nil {
		t.Fatalf("volume remove: %v (%s)", err, out)
	}
	if !strings.Contains(out, "removed cache") || !strings.Contains(out, "kept") {
		t.Fatalf("want kept-data notice, got %q", out)
	}
	out, err = run(t, "volume", "list", "web", "--project", "proj")
	if err != nil {
		t.Fatalf("volume list after remove: %v (%s)", err, out)
	}
	if strings.Contains(out, "cache") {
		t.Fatalf("volume not removed: %s", out)
	}

	// Remove with --purge on a server without kube -> 503 error surfaces.
	if _, err := run(t, "volume", "remove", "web", "data", "--project", "proj", "--purge"); err == nil {
		t.Fatal("purge without kube should fail")
	}
}
