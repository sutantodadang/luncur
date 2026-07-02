package store

import (
	"errors"
	"testing"
)

func seedProject(t *testing.T, s *Store) Project {
	t.Helper()
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAppCRUD(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if a.Port != 3000 || a.Replicas != 1 {
		t.Fatalf("bad app defaults: %+v", a)
	}

	if _, err := s.CreateApp(p.ID, "api", 3000); err == nil {
		t.Fatal("want duplicate app name error")
	}
	if _, err := s.CreateApp(p.ID, "Bad_Name", 3000); err == nil {
		t.Fatal("want invalid name error")
	}
	if _, err := s.CreateApp(p.ID, "ok", 0); err == nil {
		t.Fatal("want invalid port error")
	}

	if err := s.SetReplicas(a.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "api")
	if err != nil || got.Replicas != 3 {
		t.Fatalf("get after scale: %+v %v", got, err)
	}

	list, err := s.ListApps(p.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	if err := s.DeleteApp(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(p.ID, "api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := s.DeleteApp(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}

func TestDeployments(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.LatestDeployment(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound with no deployments, got %v", err)
	}

	d1, err := s.CreateDeployment(a.ID, "deploying", "registry/x:1")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDeployment(a.ID, "deploying", "registry/x:2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeploymentStatus(d2.ID, "live"); err != nil {
		t.Fatal(err)
	}

	latest, err := s.LatestDeployment(a.ID)
	if err != nil || latest.ID != d2.ID || latest.Status != "live" {
		t.Fatalf("latest: %+v %v (d1=%d d2=%d)", latest, err, d1.ID, d2.ID)
	}

	if _, err := s.CreateDeployment(a.ID, "bogus", "x"); err == nil {
		t.Fatal("want CHECK violation for bogus status")
	}
}
