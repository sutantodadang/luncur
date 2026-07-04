package cli

import (
	"strings"
	"testing"
)

func TestAuditCommand(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "proj"); err != nil {
		t.Fatal(err)
	}

	// The project create above is itself an audited mutation, so the log
	// already has at least one row by the time we list it.
	out, err := run(t, "audit")
	if err != nil {
		t.Fatalf("audit: %v (%s)", err, out)
	}
	for _, want := range []string{"ID", "TIME", "USER", "ACTION", "TARGET", "root@b.co", "POST /v1/projects"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in audit output, got %q", want, out)
		}
	}

	// --user filters to an exact email; a nonexistent one yields no rows
	// (just the header line).
	out, err = run(t, "audit", "--user", "nobody@b.co")
	if err != nil {
		t.Fatalf("audit --user: %v (%s)", err, out)
	}
	if strings.Contains(out, "root@b.co") {
		t.Fatalf("--user filter did not exclude other users: %q", out)
	}

	// --contains filters by substring on action/target.
	out, err = run(t, "audit", "--contains", "projects")
	if err != nil {
		t.Fatalf("audit --contains: %v (%s)", err, out)
	}
	if !strings.Contains(out, "POST /v1/projects") {
		t.Fatalf("--contains filter missing expected row: %q", out)
	}

	out, err = run(t, "audit", "--contains", "no-such-substring")
	if err != nil {
		t.Fatalf("audit --contains (no match): %v (%s)", err, out)
	}
	if strings.Contains(out, "POST /v1/projects") {
		t.Fatalf("--contains filter should have excluded everything: %q", out)
	}
}

// TestAuditCommandNonAdminForbidden checks a member's audit command surfaces
// the server's 403 as an error, mirroring other admin-only CLI commands.
func TestAuditCommandNonAdminForbidden(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "user", "add", "pleb@b.co", "--role", "member", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "login", srv.URL, "--email", "pleb@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "audit"); err == nil {
		t.Fatal("member audit: want error, got none")
	}
}
