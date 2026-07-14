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

func TestEnvPath(t *testing.T) {
	c := New("http://x", "")
	if got := c.EnvPath("web", ""); got != "/v1/projects/web" {
		t.Fatalf("empty env base = %q, want legacy /v1/projects/web", got)
	}
	if got := c.EnvPath("web", "develop"); got != "/v1/projects/web/envs/develop" {
		t.Fatalf("env base = %q, want /v1/projects/web/envs/develop", got)
	}
	// SetEnv makes app-scoped methods target the env path; empty keeps legacy.
	if c.SetEnv("develop"); c.EnvPath("web", c.env) != "/v1/projects/web/envs/develop" {
		t.Fatalf("SetEnv did not thread env into EnvPath")
	}
}

// TestAppCommandTargetsEnvPath confirms that after SetEnv, an app-scoped
// method (ListApps) hits the /envs/<env> route, while the default client
// still hits the legacy /apps route.
func TestAppCommandTargetsEnvPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, "[]")
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if _, err := c.ListApps("web"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/projects/web/apps" {
		t.Fatalf("legacy path = %q", gotPath)
	}

	c.SetEnv("develop")
	if _, err := c.ListApps("web"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/projects/web/envs/develop/apps" {
		t.Fatalf("env path = %q, want /v1/projects/web/envs/develop/apps", gotPath)
	}
}

func TestEnvCRUDClient(t *testing.T) {
	srv, st := testAPI(t)
	st.CreateUser("root@b.co", "pw123456", "admin")
	c := New(srv.URL, "")
	tok, _ := c.Login("root@b.co", "pw123456")
	c = New(srv.URL, tok)

	if _, err := c.CreateProject("web"); err != nil {
		t.Fatal(err)
	}
	// Project create seeds develop/staging/production.
	envs, err := c.ListEnvs("web")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) < 3 {
		t.Fatalf("want seeded envs, got %d: %+v", len(envs), envs)
	}

	e, err := c.CreateEnv("web", "qa", "qa")
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	if e.Name != "qa" || e.BaseBranch != "qa" {
		t.Fatalf("bad created env: %+v", e)
	}

	if err := c.SetDefaultEnv("web", "qa"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if err := c.SetPreviewBase("web", "develop"); err != nil {
		t.Fatalf("set preview base: %v", err)
	}
	// qa is now default; delete a non-default env with no apps.
	if err := c.DeleteEnv("web", "staging", false); err != nil {
		t.Fatalf("delete env: %v", err)
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

// TestRuntimeLogsRequestParams checks that follow/tail/since are encoded as
// query params on the runtime-logs request.
func TestRuntimeLogsRequestParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hi\n\n")
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := New(srv.URL, "tok")
	if err := c.RuntimeLogs("p", "a", true, 200, "15m", &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"follow=1", "tail=200", "since=15m"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
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
