package store

import (
	"errors"
	"testing"
)

func TestProjectCRUD(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	if p.Namespace != "luncur-web" || p.ID == 0 {
		t.Fatalf("bad project: %+v", p)
	}

	got, err := s.GetProject("web")
	if err != nil || got.ID != p.ID {
		t.Fatalf("get: %+v %v", got, err)
	}

	if _, err := s.GetProject("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	if _, err := s.CreateProject("web"); err == nil {
		t.Fatal("want duplicate name error")
	}

	list, err := s.ListProjects()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
}

func TestGetProjectByID(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetProjectByID(p.ID)
	if err != nil || got.Name != "web" || got.Namespace != p.Namespace {
		t.Fatalf("get by id: %+v %v", got, err)
	}
	if _, err := s.GetProjectByID(999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRenameProject(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RenameProject(p.ID, "webapp"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject("webapp")
	if err != nil || got.ID != p.ID || got.Namespace != p.Namespace {
		t.Fatalf("get renamed: %+v %v", got, err)
	}
	if _, err := s.GetProject("web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old name should be gone, got %v", err)
	}

	if err := s.RenameProject(p.ID, "-bad"); err == nil {
		t.Fatal("want validation error")
	}

	other, err := s.CreateProject("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameProject(other.ID, "webapp"); err == nil {
		t.Fatal("want duplicate name error")
	}

	if err := s.RenameProject(999999, "whatever"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteProject(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	u, err := s.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(p.ID, u.ID, "member"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProjectByID(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if ok, _ := s.IsMember(p.ID, u.ID); ok {
		t.Fatal("membership should be gone")
	}

	if err := s.DeleteProject(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on redelete, got %v", err)
	}
}

func TestProjectDefaultAndPreviewBaseEnv(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetProject("web")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultEnv != "production" {
		t.Fatalf("default_env = %q, want production", got.DefaultEnv)
	}
	if got.PreviewBaseEnv != "develop" {
		t.Fatalf("preview_base_env = %q, want develop", got.PreviewBaseEnv)
	}

	if err := s.SetDefaultEnv(p.ID, "staging"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPreviewBaseEnv(p.ID, "staging"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProject("web")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultEnv != "staging" || got.PreviewBaseEnv != "staging" {
		t.Fatalf("after set: %+v", got)
	}

	if err := s.SetDefaultEnv(p.ID, "-bad"); err == nil {
		t.Fatal("want validation error for bad default env name")
	}
	if err := s.SetPreviewBaseEnv(p.ID, "-bad"); err == nil {
		t.Fatal("want validation error for bad preview base env name")
	}

	if err := s.SetDefaultEnv(999999, "production"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: want ErrNotFound, got %v", err)
	}
	if err := s.SetPreviewBaseEnv(999999, "develop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: want ErrNotFound, got %v", err)
	}
}

func TestCreateProjectValidatesName(t *testing.T) {
	s := openTest(t)
	for _, bad := range []string{"", "-x", "x-", "UPPER", "has_underscore", "waaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaytoolong"} {
		if _, err := s.CreateProject(bad); err == nil {
			t.Errorf("name %q: want error", bad)
		}
	}
	for _, good := range []string{"a", "web-1", "my-app"} {
		if _, err := s.CreateProject(good); err != nil {
			t.Errorf("name %q: %v", good, err)
		}
	}
}
