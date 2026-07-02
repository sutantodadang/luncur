package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

// httptestServer names the concrete type newHTTPTest returns, for helpers
// (e.g. apps_test.go's kubeServer) that need to spell it in a signature.
type httptestServer = httptest.Server

// newTestStore opens a fresh SQLite store backed by a temp file.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newHTTPTest builds a test HTTP server from arbitrary Deps.
func newHTTPTest(t *testing.T, d Deps) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(d))
	t.Cleanup(srv.Close)
	return srv
}

func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	return srv, st
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealth(t *testing.T) {
	srv, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestLogin(t *testing.T) {
	srv, st := testServer(t)
	if _, err := st.CreateUser("a@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, srv.URL+"/v1/login", `{"email":"a@b.co","password":"pw123456"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Token, "lcr_") {
		t.Fatalf("bad token: %q", out.Token)
	}

	bad := postJSON(t, srv.URL+"/v1/login", `{"email":"a@b.co","password":"nope"}`)
	defer bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Fatalf("want 401, got %d", bad.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bad.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "auth_failed" {
		t.Fatalf("want code auth_failed, got %q", env.Error.Code)
	}
}

func TestUnknownPathReturns404Envelope(t *testing.T) {
	srv, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/v1/no-such-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "not_found" {
		t.Fatalf("want code not_found, got %q", env.Error.Code)
	}
}
