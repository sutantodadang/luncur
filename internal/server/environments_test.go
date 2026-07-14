package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

// TestRequireEnv covers requireEnv's default-env fallback, explicit
// resolution, and 404s on a missing environment or missing project.
func TestRequireEnv(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})
	admin := store.User{Role: "admin"}

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := st.CreateEnvironment(p.ID, "production", "standing", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultEnvironment(p.ID, prod.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultEnv(p.ID, "production"); err != nil {
		t.Fatal(err)
	}
	dev, err := st.CreateEnvironment(p.ID, "develop", "standing", "develop")
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)

	t.Run("empty env resolves to project default", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, env, ok := srv.requireEnv(w, r, admin, "proj", "")
		if !ok {
			t.Fatalf("requireEnv failed: %d %s", w.Code, w.Body.String())
		}
		if env.ID != prod.ID {
			t.Fatalf("env = %+v, want production", env)
		}
	})

	t.Run("explicit env name resolves that env", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, env, ok := srv.requireEnv(w, r, admin, "proj", "develop")
		if !ok {
			t.Fatalf("requireEnv failed: %d %s", w.Code, w.Body.String())
		}
		if env.ID != dev.ID {
			t.Fatalf("env = %+v, want develop", env)
		}
	})

	t.Run("missing env 404s", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, _, ok := srv.requireEnv(w, r, admin, "proj", "staging")
		if ok {
			t.Fatal("want not ok for missing env")
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})

	t.Run("missing project 404s", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, _, ok := srv.requireEnv(w, r, admin, "nope", "")
		if ok {
			t.Fatal("want not ok for missing project")
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
}

// TestRequireEnvWrite covers requireEnvWrite's write-role check: a viewer
// is denied with 403 read_only, same as requireProjectWrite.
func TestRequireEnvWrite(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := st.CreateEnvironment(p.ID, "production", "standing", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultEnvironment(p.ID, prod.ID); err != nil {
		t.Fatal(err)
	}

	viewer, err := st.CreateUser("viewer@x.co", "password1", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(p.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	_, _, ok := srv.requireEnvWrite(w, r, viewer, "proj", "")
	if ok {
		t.Fatal("want viewer denied write")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// decodeEnvs is TestEnvCRUD's small helper: decode a GET .../envs response
// body into a name->row map so assertions can look up a specific env
// without caring about list order.
func decodeEnvs(t *testing.T, resp *http.Response) map[string]map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode envs: %v", err)
	}
	out := make(map[string]map[string]any, len(list))
	for _, e := range list {
		out[e["name"].(string)] = e
	}
	return out
}

// TestEnvCRUD covers Task 8's handlers end to end over HTTP: list (the 3
// seeded envs), create, reject-duplicate, refuse deleting the default env,
// refuse deleting an env with live apps unless ?force=1, and set-default
// reassignment.
func TestEnvCRUD(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p"}`).Body.Close()

	// List: project create seeds develop/staging/production (Task 7).
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/p/envs", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list envs: want 200, got %d", resp.StatusCode)
	}
	envs := decodeEnvs(t, resp)
	if len(envs) != 3 {
		t.Fatalf("want 3 seeded envs, got %+v", envs)
	}
	if !envs["production"]["is_default"].(bool) {
		t.Fatalf("production should be default: %+v", envs["production"])
	}

	// Create.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/p/envs", admin, `{"name":"qa"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create env: want 201, got %d", resp.StatusCode)
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created["namespace"] != "luncur-p-qa" || created["kind"] != "standing" {
		t.Fatalf("created env: %+v", created)
	}

	// Duplicate name -> 400.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/p/envs", admin, `{"name":"qa"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate env: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Delete the default env -> 409.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/p/envs/production", admin, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete default env: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Delete a non-default, app-free env -> 204.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/p/envs/qa", admin, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete empty env: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// An env with a live app refuses deletion unless ?force=1. The
	// env-scoped app-create route lands in Task 9; here the store creates
	// the app directly in "develop" (bypassing the still-legacy-only HTTP
	// app routes) purely to populate ListAppsInEnv for this check.
	proj, err := st.GetProject("p")
	if err != nil {
		t.Fatal(err)
	}
	developEnv, err := st.GetEnvironment(proj.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAppInEnv(developEnv.ID, "api", 3000, "web", ""); err != nil {
		t.Fatal(err)
	}

	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/p/envs/develop", admin, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete env with apps: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/p/envs/develop?force=1", admin, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("force delete env with apps: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Set-default reassigns.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/p/envs/staging/default", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set default: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	envs = decodeEnvs(t, doAuthed(t, "GET", srv.URL+"/v1/projects/p/envs", admin, ""))
	if !envs["staging"]["is_default"].(bool) {
		t.Fatalf("staging should now be default: %+v", envs["staging"])
	}
	if envs["production"]["is_default"].(bool) {
		t.Fatalf("production should no longer be default: %+v", envs["production"])
	}
}

// TestSetPreviewBase covers handleSetPreviewBase: it accepts an existing
// env name and 404s on an unknown one.
func TestSetPreviewBase(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p"}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p/preview-base", admin, `{"env":"staging"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set preview base: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/p/preview-base", admin, `{"env":"nope"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("set preview base unknown env: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
