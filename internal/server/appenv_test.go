package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func seedWebApi(t *testing.T, srv *httptestServer, admin string) {
	t.Helper()
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
}

func TestEnvRoundTrip(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	seedWebApi(t, srv, admin)

	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/env", admin, `{"key":"DB_URL","value":"postgres://x"}`); resp.StatusCode != 204 {
		t.Fatalf("put env: want 204, got %d", resp.StatusCode)
	}
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/env", admin, "")
	var env map[string]string
	json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env["DB_URL"] != "postgres://x" {
		t.Fatalf("env: %v", env)
	}
	// Sealed at rest: raw store bytes must not contain plaintext.
	var raw []byte
	st.DB().QueryRow(`SELECT value_enc FROM env_vars LIMIT 1`).Scan(&raw)
	if strings.Contains(string(raw), "postgres") {
		t.Fatal("env value stored unsealed")
	}
	if resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/env/DB_URL", admin, ""); resp.StatusCode != 204 {
		t.Fatalf("delete env: want 204, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api/env/DB_URL", admin, ""); resp.StatusCode != 404 {
		t.Fatalf("second delete: want 404, got %d", resp.StatusCode)
	}
}

func TestOverrideAndRaw(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	seedWebApi(t, srv, admin)

	patch := `{"metadata":{"labels":{"team":"x"}}}`
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/overrides/Deployment", admin, patch); resp.StatusCode != 204 {
		t.Fatalf("put override: want 204, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/overrides/Pod", admin, `{}`); resp.StatusCode != 400 {
		t.Fatalf("bad kind: want 400, got %d", resp.StatusCode)
	}

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/raw", admin, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("content type: %s", ct)
	}
	if !strings.Contains(string(body), "team: x") {
		t.Fatalf("override missing from raw:\n%s", body)
	}

	respBase := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/raw?base=1", admin, "")
	baseBody, _ := io.ReadAll(respBase.Body)
	respBase.Body.Close()
	if strings.Contains(string(baseBody), "team: x") {
		t.Fatal("base render must exclude overrides")
	}
}
