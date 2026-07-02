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

func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st))
	t.Cleanup(func() { srv.Close(); st.Close() })
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
	var out struct{ Token string `json:"token"` }
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
