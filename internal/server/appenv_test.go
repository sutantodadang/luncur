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

// TestRenderInjectsPortEnv: apps built from source follow the buildpack
// contract and bind to $PORT. renderApp must inject PORT=<app.port> so the
// container listens where the Service targets — and a user-set PORT wins.
func TestRenderInjectsPortEnv(t *testing.T) {
	s, srv, st, _ := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}

	secretJSON := func(t *testing.T) string {
		t.Helper()
		rendered, err := s.renderApp(p, a, "nginx:1", true)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range rendered.Objects {
			if o.Kind == "Secret" {
				return string(o.JSON)
			}
		}
		t.Fatal("no Secret object rendered")
		return ""
	}

	if sec := secretJSON(t); !strings.Contains(sec, `"PORT":"8080"`) {
		t.Fatalf("rendered secret missing injected PORT: %s", sec)
	}

	// User-set PORT wins over the injected default.
	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/proj/apps/web/env", admin, `{"key":"PORT","value":"9999"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("set env: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if sec := secretJSON(t); !strings.Contains(sec, `"PORT":"9999"`) {
		t.Fatalf("user PORT should win over injected default: %s", sec)
	}
}
