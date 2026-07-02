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
	if _, err := s.CreateUser("a@b.co", "pw", "superuser"); err == nil {
		t.Fatal("want error for invalid role")
	}
	if _, err := s.CreateUser("", "pw", "member"); err == nil {
		t.Fatal("want error for empty email")
	}
	if _, _ = s.CreateUser("dup@b.co", "pw", "member"); true {
		if _, err := s.CreateUser("dup@b.co", "pw", "member"); err == nil {
			t.Fatal("want error for duplicate email")
		}
	}
}
