package store

import "testing"

func TestEnvironmentCRUD(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}

	e, err := s.CreateEnvironment(p.ID, "develop", "standing", "develop")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.Namespace != "luncur-proj-develop" {
		t.Fatalf("ns = %q", e.Namespace)
	}
	if e.IsDefault {
		t.Fatal("new env should not be default")
	}

	if _, err := s.CreateEnvironment(p.ID, "develop", "standing", ""); err == nil {
		t.Fatal("want duplicate-name error")
	}

	got, err := s.GetEnvironment(p.ID, "develop")
	if err != nil || got.ID != e.ID {
		t.Fatalf("get: %v %+v", err, got)
	}

	if err := s.SetDefaultEnvironment(p.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetEnvironment(p.ID, "develop")
	if !got.IsDefault {
		t.Fatal("want default after set")
	}

	e2, _ := s.CreateEnvironment(p.ID, "staging", "standing", "")
	if err := s.SetDefaultEnvironment(p.ID, e2.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetEnvironment(p.ID, "develop")
	if got.IsDefault {
		t.Fatal("old default must be cleared")
	}

	if err := s.DeleteEnvironment(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEnvironment(p.ID, "develop"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
