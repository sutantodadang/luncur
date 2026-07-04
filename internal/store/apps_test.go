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

func TestGetAppByID(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetAppByID(a.ID)
	if err != nil || got.Name != "api" || got.ProjectID != p.ID {
		t.Fatalf("get by id: %+v %v", got, err)
	}
	if _, err := s.GetAppByID(999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateGitApp(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	a, err := s.CreateGitApp(p.ID, "web", 8080, "https://example.com/repo.git", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceType != "git" || a.GitURL != "https://example.com/repo.git" || a.GitBranch != "main" {
		t.Fatalf("bad git app: %+v", a)
	}

	got, err := s.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceType != "git" || got.GitURL != "https://example.com/repo.git" || got.GitBranch != "main" {
		t.Fatalf("get after create: %+v", got)
	}

	if _, err := s.CreateGitApp(p.ID, "web2", 8080, "", "main"); err == nil {
		t.Fatal("want error for empty git url")
	}

	explicit, err := s.CreateGitApp(p.ID, "web3", 8080, "https://example.com/repo2.git", "develop")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.GitBranch != "develop" {
		t.Fatalf("want explicit branch preserved, got %+v", explicit)
	}
}

func TestCreateAppSourceType(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceType != "tarball" {
		t.Fatalf("want SourceType=tarball, got %+v", a)
	}

	got, err := s.GetApp(p.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceType != "tarball" || got.GitURL != "" || got.GitBranch != "" {
		t.Fatalf("get after create: %+v", got)
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

	d1, err := s.CreateDeployment(a.ID, "deploying", "registry/x:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDeployment(a.ID, "deploying", "registry/x:2", 0)
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

	if _, err := s.CreateDeployment(a.ID, "bogus", "x", 0); err == nil {
		t.Fatal("want CHECK violation for bogus status")
	}
}

func TestAppEjected(t *testing.T) {
	s := openTest(t)
	p, _ := s.CreateProject("proj")
	a, _ := s.CreateApp(p.ID, "web", 8080)
	if a.Ejected {
		t.Fatal("new app born ejected")
	}
	if err := s.SetAppEjected(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "web")
	if err != nil || !got.Ejected {
		t.Fatalf("ejected not persisted: %+v err=%v", got, err)
	}
	byID, err := s.GetAppByID(a.ID)
	if err != nil || !byID.Ejected {
		t.Fatalf("GetAppByID: %+v err=%v", byID, err)
	}
	list, err := s.ListApps(p.ID)
	if err != nil || len(list) != 1 || !list[0].Ejected {
		t.Fatalf("ListApps: %+v err=%v", list, err)
	}
	if err := s.SetAppEjected(a.ID + 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing app: %v", err)
	}
}

func TestSetAppAdopted(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetAppEjected(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ejected {
		t.Fatal("want ejected after SetAppEjected")
	}

	if err := s.SetAppAdopted(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ejected {
		t.Fatal("want not ejected after SetAppAdopted")
	}

	if err := s.SetAppAdopted(99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}
