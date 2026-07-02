package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	srv := httptest.NewServer(server.New(st))
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

func TestWhoamiWithoutLogin(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if _, err := run(t, "whoami"); err == nil {
		t.Fatal("want error when not logged in")
	}
}
