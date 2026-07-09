package store

import (
	"errors"
	"testing"
)

func TestMembers(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	u, err := s.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.IsMember(p.ID, u.ID); ok {
		t.Fatal("not a member yet")
	}
	if err := s.AddMember(p.ID, u.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(p.ID, u.ID, "member"); err != nil {
		t.Fatal("second add must be idempotent")
	}
	if ok, _ := s.IsMember(p.ID, u.ID); !ok {
		t.Fatal("want member")
	}

	got, err := s.GetUserByEmail("m@b.co")
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetUserByEmail: %+v %v", got, err)
	}

	members, err := s.ListMembers(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Email != "m@b.co" {
		t.Fatalf("ListMembers = %+v, want one m@b.co entry", members)
	}
}

func TestRemoveMember(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	u, err := s.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(p.ID, u.ID, "member"); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveMember(p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsMember(p.ID, u.ID); ok {
		t.Fatal("want not a member")
	}

	if err := s.RemoveMember(p.ID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMemberRoles(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	u, _ := s.CreateUser("v@x.io", "pw123456", "member")

	if err := s.AddMember(p.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	role, err := s.MemberRole(p.ID, u.ID)
	if err != nil || role != "viewer" {
		t.Fatalf("role=%q err=%v", role, err)
	}
	if err := s.AddMember(p.ID, u.ID, "owner"); err == nil {
		t.Fatal("invalid role must be rejected")
	}
}
