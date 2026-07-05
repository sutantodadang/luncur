package cli

import (
	"strings"
	"testing"
)

// TestAppCreateKindFlagMatrix exercises `app create`'s --kind/--schedule
// flags: web (default, unchanged), worker (no --port needed), and cron
// (requires --schedule).
func TestAppCreateKindFlagMatrix(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}

	// web (default kind) unchanged.
	out, err := run(t, "app", "create", "api", "--project", "p", "--port", "3000")
	if err != nil {
		t.Fatalf("web create: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kind web") {
		t.Fatalf("web create output: %q", out)
	}

	// worker: no --port needed.
	out, err = run(t, "app", "create", "worker1", "--project", "p", "--kind", "worker")
	if err != nil {
		t.Fatalf("worker create: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kind worker") {
		t.Fatalf("worker create output: %q", out)
	}

	// cron without --schedule: server rejects.
	if _, err := run(t, "app", "create", "bad-cron", "--project", "p", "--kind", "cron"); err == nil {
		t.Fatal("want error creating cron app without --schedule")
	}

	// cron with --schedule: happy path.
	out, err = run(t, "app", "create", "nightly", "--project", "p", "--kind", "cron", "--schedule", "0 3 * * *")
	if err != nil {
		t.Fatalf("cron create: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kind cron") {
		t.Fatalf("cron create output: %q", out)
	}
}

// TestAppCreateBuildPathFlag checks that `app create --path` round-trips
// through the API into the stored build_path (not surfaced via app info/list
// responses, mirroring how git_url isn't either — verified directly against
// the store).
func TestAppCreateBuildPathFlag(t *testing.T) {
	srv, st := testEnvStore(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "api", "--project", "p", "--port", "3000", "--path", "backend"); err != nil {
		t.Fatal(err)
	}

	proj, err := st.GetProject("p")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(proj.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.BuildPath != "backend" {
		t.Fatalf("build path: got %q, want %q", a.BuildPath, "backend")
	}
}

// TestAppCreateInternalFlag checks that `app create --internal` round-trips
// through the API into the stored internal flag (mirroring
// TestAppCreateBuildPathFlag's style), and that combining it with
// --kind worker is rejected server-side.
func TestAppCreateInternalFlag(t *testing.T) {
	srv, st := testEnvStore(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "ai", "--project", "p", "--port", "8001", "--internal"); err != nil {
		t.Fatal(err)
	}

	proj, err := st.GetProject("p")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(proj.ID, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Internal {
		t.Fatalf("internal: got %v, want true", a.Internal)
	}

	if _, err := run(t, "app", "create", "w1", "--project", "p", "--kind", "worker", "--internal"); err == nil {
		t.Fatal("want error creating internal worker app")
	}
}

// TestAppListShowsKind checks `app list`'s KIND column and that non-web apps
// show "-" instead of a URL.
func TestAppListShowsKind(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "api", "--project", "p", "--port", "3000"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "worker1", "--project", "p", "--kind", "worker"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "app", "list", "--project", "p")
	if err != nil {
		t.Fatalf("app list: %v (%s)", err, out)
	}
	for _, want := range []string{"api", "web", "worker1", "worker", "-"} {
		if !strings.Contains(out, want) {
			t.Fatalf("app list missing %q: %q", want, out)
		}
	}
}

// TestAppInfoShowsKindAndSchedule checks `app info` prints the kind, and the
// schedule for cron apps.
func TestAppInfoShowsKindAndSchedule(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "nightly", "--project", "p", "--kind", "cron", "--schedule", "0 3 * * *"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "app", "info", "nightly", "--project", "p")
	if err != nil {
		t.Fatalf("app info: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kind=cron") || !strings.Contains(out, "schedule=0 3 * * *") {
		t.Fatalf("app info output: %q", out)
	}
}
