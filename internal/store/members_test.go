package store

import "testing"

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
	if err := s.AddMember(p.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(p.ID, u.ID); err != nil {
		t.Fatal("second add must be idempotent")
	}
	if ok, _ := s.IsMember(p.ID, u.ID); !ok {
		t.Fatal("want member")
	}

	got, err := s.GetUserByEmail("m@b.co")
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetUserByEmail: %+v %v", got, err)
	}
}
