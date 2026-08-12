package store

import (
	"errors"
	"testing"
)

func seedApp(t *testing.T, s *Store) App {
	t.Helper()
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestEnvVars(t *testing.T) {
	s := openTest(t)
	a := seedApp(t, s)

	if err := s.SetEnv(a.ID, "DB_URL", []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	// Upsert overwrites.
	if err := s.SetEnv(a.ID, "DB_URL", []byte("sealed-2")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(a.ID, "lowercase", []byte("x")); err == nil {
		t.Fatal("want error for invalid key")
	}

	env, err := s.ListEnv(a.ID)
	if err != nil || len(env) != 1 || string(env["DB_URL"]) != "sealed-2" {
		t.Fatalf("list: %v %v", env, err)
	}

	if err := s.UnsetEnv(a.ID, "DB_URL"); err != nil {
		t.Fatal(err)
	}
	if err := s.UnsetEnv(a.ID, "DB_URL"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestOverrides(t *testing.T) {
	s := openTest(t)
	a := seedApp(t, s)

	patch := `{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"256Mi"}}}]}}}}`
	if err := s.SetOverride(a.ID, "Deployment", patch); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces.
	if err := s.SetOverride(a.ID, "Deployment", `{"metadata":{"labels":{"x":"y"}}}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOverride(a.ID, "Pod", `{}`); err == nil {
		t.Fatal("want error for unsupported kind")
	}
	if err := s.SetOverride(a.ID, "Service", `not json`); err == nil {
		t.Fatal("want error for invalid JSON")
	}

	m, err := s.Overrides(a.ID)
	if err != nil || len(m) != 1 || m["Deployment"] != `{"metadata":{"labels":{"x":"y"}}}` {
		t.Fatalf("overrides: %v %v", m, err)
	}

	if err := s.DeleteOverride(a.ID, "Deployment"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOverride(a.ID, "Deployment"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestReplaceEnv proves ReplaceEnv swaps an app's whole var set atomically:
// vars missing from the new set are deleted, overlapping keys take the new
// sealed bytes, and an invalid key rejects the whole call without wiping
// the existing set.
func TestReplaceEnv(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "app", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(a.ID, "OLD_ONLY", []byte("sealed-old")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(a.ID, "SHARED", []byte("sealed-v1")); err != nil {
		t.Fatal(err)
	}

	if err := s.ReplaceEnv(a.ID, map[string][]byte{
		"SHARED":   []byte("sealed-v2"),
		"NEW_ONLY": []byte("sealed-new"),
	}); err != nil {
		t.Fatal(err)
	}
	vars, err := s.ListEnv(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 {
		t.Fatalf("want 2 vars after replace, got %v", vars)
	}
	if string(vars["SHARED"]) != "sealed-v2" || string(vars["NEW_ONLY"]) != "sealed-new" {
		t.Fatalf("bad vars after replace: %v", vars)
	}
	if _, ok := vars["OLD_ONLY"]; ok {
		t.Fatal("OLD_ONLY should have been deleted")
	}

	// Invalid key: whole call rejected, existing set untouched.
	if err := s.ReplaceEnv(a.ID, map[string][]byte{"bad-key": []byte("x")}); err == nil {
		t.Fatal("want error for invalid key")
	}
	vars, err = s.ListEnv(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 {
		t.Fatalf("invalid replace must not change vars, got %v", vars)
	}
}
