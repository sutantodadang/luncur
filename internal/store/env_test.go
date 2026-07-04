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
