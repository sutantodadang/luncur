package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

func testEnv(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("root@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{Store: st, Sealer: sealer}))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestLoginWhoamiUserAdd(t *testing.T) {
	srv := testEnv(t)

	out, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456")
	if err != nil {
		t.Fatalf("login: %v (%s)", err, out)
	}
	if !strings.Contains(out, "logged in") {
		t.Fatalf("want 'logged in', got %q", out)
	}

	out, err = run(t, "whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(out, "root@b.co (admin)") {
		t.Fatalf("want identity line, got %q", out)
	}

	out, err = run(t, "user", "add", "new@b.co", "--role", "member", "--password", "pw123456")
	if err != nil {
		t.Fatalf("user add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "new@b.co") {
		t.Fatalf("want created email in output, got %q", out)
	}
}

func TestLoginPromptsForEmail(t *testing.T) {
	srv := testEnv(t)

	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader("root@b.co\n"))
	root.SetArgs([]string{"login", srv.URL, "--password", "pw123456"})
	if err := root.Execute(); err != nil {
		t.Fatalf("login with prompted email: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "email: ") {
		t.Fatalf("want email prompt, got %q", out.String())
	}
	if !strings.Contains(out.String(), "logged in") {
		t.Fatalf("want 'logged in', got %q", out.String())
	}

	got, err := run(t, "whoami")
	if err != nil {
		t.Fatalf("whoami after prompted login: %v", err)
	}
	if !strings.Contains(got, "root@b.co (admin)") {
		t.Fatalf("want identity line, got %q", got)
	}
}

func TestWhoamiWithoutLogin(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if _, err := run(t, "whoami"); err == nil {
		t.Fatal("want error when not logged in")
	}
}

func TestStatusAppAndList(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "api", "--project", "web", "--port", "3000"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "status", "api", "--project", "web")
	if err != nil {
		t.Fatalf("status app: %v (%s)", err, out)
	}
	for _, want := range []string{"app:      api", "status:   never_deployed", "replicas: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in output, got %q", want, out)
		}
	}

	out, err = run(t, "status", "--project", "web")
	if err != nil {
		t.Fatalf("status list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "api") {
		t.Fatalf("want app list, got %q", out)
	}
}
