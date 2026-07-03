package store

import (
	"errors"
	"testing"
)

func TestCreateAndAuthenticateUser(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("a@b.co", "s3cret-pw", "admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Email != "a@b.co" || u.Role != "admin" || u.ID == 0 {
		t.Fatalf("bad user: %+v", u)
	}

	got, err := s.Authenticate("a@b.co", "s3cret-pw")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("want id %d, got %d", u.ID, got.ID)
	}

	if _, err := s.Authenticate("a@b.co", "wrong"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed, got %v", err)
	}
	if _, err := s.Authenticate("nobody@b.co", "s3cret-pw"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed for unknown email, got %v", err)
	}
}

func TestCreateUserRejectsBadInput(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateUser("a@b.co", "longpw12", "superuser"); err == nil {
		t.Fatal("want error for invalid role")
	}
	if _, err := s.CreateUser("", "longpw12", "member"); err == nil {
		t.Fatal("want error for empty email")
	}
	if _, err := s.CreateUser("dup@b.co", "longpw12", "member"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateUser("dup@b.co", "longpw12", "member"); err == nil {
		t.Fatal("want error for duplicate email")
	}
}

func TestCreateUserRejectsShortPassword(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateUser("short@b.co", "pw", "member"); err == nil {
		t.Fatal("want error for short password")
	}
}

func TestListAndDeleteUsers(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateUser("a@example.com", "password123", "admin")
	b, _ := s.CreateUser("b@example.com", "password123", "member")
	if _, err := s.CreateToken(b.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUsers()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].ID != a.ID || list[1].TokenCount != 1 || list[0].TokenCount != 0 {
		t.Fatalf("rows: %+v", list)
	}
	if err := s.DeleteUser(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Cascade: b's token is gone.
	if l, _ := s.ListTokens(b.ID); len(l) != 0 {
		t.Fatalf("tokens survived user delete: %+v", l)
	}
}
