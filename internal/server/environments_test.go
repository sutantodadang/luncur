package server

import (
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
