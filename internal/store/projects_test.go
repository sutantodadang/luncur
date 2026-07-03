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
