package store

import (
	"errors"
	"testing"
)

func TestInviteLifecycle(t *testing.T) {
	s := openTest(t)
	admin, _ := s.CreateUser("admin@example.com", "password123", "admin")

	inv, err := s.CreateInvite("member", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Token) != 32 || inv.Role != "member" || inv.ExpiresAt == "" {
		t.Fatalf("invite = %+v", inv)
	}
	if _, err := s.CreateInvite("owner", admin.ID); err == nil {
		t.Fatal("bad role accepted")
	}

	got, err := s.GetValidInvite(inv.Token)
	if err != nil || got.Role != "member" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := s.GetValidInvite("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}

	list, err := s.ListInvites()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	u, _ := s.CreateUser("new@example.com", "password123", "member")
	if err := s.MarkInviteUsed(inv.Token, u.ID); err != nil {
		t.Fatal(err)
	}
	// Used invites stop validating and can't be used twice.
	if _, err := s.GetValidInvite(inv.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("used invite still valid: %v", err)
	}
	if err := s.MarkInviteUsed(inv.Token, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double-use: %v", err)
	}

	// Expired invites don't validate.
	exp, _ := s.CreateInvite("member", admin.ID)
	if _, err := s.db.Exec(
		`UPDATE invites SET expires_at = datetime('now', '-1 day') WHERE token = ?`, exp.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetValidInvite(exp.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invite valid: %v", err)
	}

	if err := s.RevokeInvite(exp.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(exp.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}
