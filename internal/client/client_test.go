package client

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

func testAPI(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{Store: st}))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, st
}

func TestClientLoginMeCreateUser(t *testing.T) {
	srv, st := testAPI(t)
	if _, err := st.CreateUser("root@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}

	c := New(srv.URL, "")
	tok, err := c.Login("root@b.co", "pw123456")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	c = New(srv.URL, tok)
	me, err := c.Me()
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if me.Email != "root@b.co" || me.Role != "admin" {
		t.Fatalf("bad me: %+v", me)
	}

	nu, err := c.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if nu.Email != "m@b.co" {
		t.Fatalf("bad created user: %+v", nu)
	}
}

func TestClientSurfacesAPIErrors(t *testing.T) {
	srv, _ := testAPI(t)
	c := New(srv.URL, "")
	_, err := c.Login("ghost@b.co", "nope")
	if err == nil || !strings.Contains(err.Error(), "auth_failed") {
		t.Fatalf("want auth_failed in error, got %v", err)
	}
}
