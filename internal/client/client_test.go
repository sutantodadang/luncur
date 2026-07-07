package client

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

func testAPI(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{Store: st, Sealer: sealer}))
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

func TestClientProjectAppEnvRawFlow(t *testing.T) {
	srv, st := testAPI(t)
	st.CreateUser("root@b.co", "pw123456", "admin")
	c := New(srv.URL, "")
	tok, _ := c.Login("root@b.co", "pw123456")
	c = New(srv.URL, tok)

	if _, err := c.CreateProject("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateApp("web", "api", 3000, "web", "", "", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.EnvSet("web", "api", "K", "v"); err != nil {
		t.Fatal(err)
	}
	env, err := c.EnvList("web", "api")
	if err != nil || env["K"] != "v" {
		t.Fatalf("env: %v %v", env, err)
	}
	if err := c.PutOverride("web", "api", "Deployment", `{"metadata":{"labels":{"t":"x"}}}`); err != nil {
		t.Fatal(err)
	}
	y, err := c.Raw("web", "api", false)
	if err != nil || !strings.Contains(string(y), "t: x") {
		t.Fatalf("raw: %v\n%s", err, y)
	}
	if err := c.DeleteApp("web", "api"); err == nil || !strings.Contains(err.Error(), "kubernetes_unavailable") {
		t.Fatalf("want kubernetes_unavailable, got %v", err)
	}
}

func TestStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hello\n\ndata: world\n\nevent: end\ndata: live\n\n")
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := New(srv.URL, "tok")
	if err := c.FollowDeployLogs("p", "a", "1", &buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "hello\nworld\n" {
		t.Fatalf("got %q", got)
	}
}
