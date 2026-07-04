package store

import (
	"errors"
	"strings"
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

	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Port != 3000 || a.Replicas != 1 {
		t.Fatalf("bad app defaults: %+v", a)
	}

	if _, err := s.CreateApp(p.ID, "api", 3000, "web", ""); err == nil {
		t.Fatal("want duplicate app name error")
	}
	if _, err := s.CreateApp(p.ID, "Bad_Name", 3000, "web", ""); err == nil {
		t.Fatal("want invalid name error")
	}
	if _, err := s.CreateApp(p.ID, "ok", 0, "web", ""); err == nil {
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
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
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

	a, err := s.CreateGitApp(p.ID, "web", 8080, "https://example.com/repo.git", "", "web", "")
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

	if _, err := s.CreateGitApp(p.ID, "web2", 8080, "", "main", "web", ""); err == nil {
		t.Fatal("want error for empty git url")
	}

	explicit, err := s.CreateGitApp(p.ID, "web3", 8080, "https://example.com/repo2.git", "develop", "web", "")
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

	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
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
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
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
	a, _ := s.CreateApp(p.ID, "web", 8080, "web", "")
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
	a, err := s.CreateApp(p.ID, "web", 8080, "web", "")
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

func TestSetResources(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.CPUMilli != 0 || a.MemoryMB != 0 {
		t.Fatalf("want unset resources by default, got %+v", a)
	}

	if err := s.SetResources(a.ID, 250, 256); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "api")
	if err != nil || got.CPUMilli != 250 || got.MemoryMB != 256 {
		t.Fatalf("get after set resources: %+v %v", got, err)
	}

	if err := s.SetResources(a.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetApp(p.ID, "api")
	if err != nil || got.CPUMilli != 0 || got.MemoryMB != 0 {
		t.Fatalf("get after clear resources: %+v %v", got, err)
	}

	if err := s.SetResources(a.ID, -1, 0); err == nil {
		t.Fatal("want error for negative cpu")
	}
	if err := s.SetResources(a.ID, 0, -1); err == nil {
		t.Fatal("want error for negative memory")
	}

	if err := s.SetResources(99999, 100, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}

func TestCreateAppKindMatrix(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	// worker with a port is invalid.
	if _, err := s.CreateApp(p.ID, "w1", 8080, "worker", ""); err == nil {
		t.Fatal("want error: worker apps do not take a port")
	}
	// cron without a schedule is invalid.
	if _, err := s.CreateApp(p.ID, "c1", 0, "cron", ""); err == nil {
		t.Fatal("want error: cron apps require a schedule")
	}
	// web with a schedule is invalid.
	if _, err := s.CreateApp(p.ID, "web1", 8080, "web", "* * * * *"); err == nil {
		t.Fatal("want error: schedule only valid for cron")
	}
	// unknown kind is invalid.
	if _, err := s.CreateApp(p.ID, "bad1", 0, "bogus", ""); err == nil {
		t.Fatal("want error: invalid kind")
	}

	// worker happy path.
	w, err := s.CreateApp(p.ID, "worker1", 0, "worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Kind != "worker" || w.Port != 0 {
		t.Fatalf("bad worker app: %+v", w)
	}

	// cron happy path.
	c, err := s.CreateApp(p.ID, "cron1", 0, "cron", "0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "cron" || c.Schedule != "0 3 * * *" {
		t.Fatalf("bad cron app: %+v", c)
	}

	// "" kind normalizes to web.
	def, err := s.CreateApp(p.ID, "defaultkind", 8080, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if def.Kind != "web" {
		t.Fatalf("want kind normalized to web, got %+v", def)
	}

	got, err := s.GetApp(p.ID, "cron1")
	if err != nil || got.Kind != "cron" || got.Schedule != "0 3 * * *" {
		t.Fatalf("get after create: %+v %v", got, err)
	}
}

func TestSetHealthPath(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.HealthPath != "" {
		t.Fatalf("want unset health path by default, got %+v", a)
	}

	if err := s.SetHealthPath(a.ID, "/healthz"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "api")
	if err != nil || got.HealthPath != "/healthz" {
		t.Fatalf("get after set health path: %+v %v", got, err)
	}

	if err := s.SetHealthPath(a.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetApp(p.ID, "api")
	if err != nil || got.HealthPath != "" {
		t.Fatalf("get after clear health path: %+v %v", got, err)
	}

	if err := s.SetHealthPath(a.ID, "healthz"); err == nil {
		t.Fatal("want error for missing leading slash")
	}
	if err := s.SetHealthPath(a.ID, "/"+strings.Repeat("a", 256)); err == nil {
		t.Fatal("want error for path over 256 chars")
	}
	if err := s.SetHealthPath(a.ID, "/health z"); err == nil {
		t.Fatal("want error for embedded space")
	}

	if err := s.SetHealthPath(99999, "/healthz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}
