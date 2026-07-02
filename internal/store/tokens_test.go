package store

import (
	"errors"
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("t@b.co", "pw", "member")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.CreateToken(u.ID, "cli")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !strings.HasPrefix(tok, "lcr_") || len(tok) != 4+64 {
		t.Fatalf("bad token format: %q", tok)
	}

	got, err := s.UserForToken(tok)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("want user %d, got %d", u.ID, got.ID)
	}

	// Plaintext must not be stored anywhere.
	var n int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM api_tokens WHERE hash = ?`, tok,
	).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext token stored in DB (n=%d err=%v)", n, err)
	}
}

func TestUserForTokenRejectsUnknown(t *testing.T) {
	s := openTest(t)
	for _, bad := range []string{"", "lcr_deadbeef", "not-a-token"} {
		if _, err := s.UserForToken(bad); !errors.Is(err, ErrAuthFailed) {
			t.Errorf("token %q: want ErrAuthFailed, got %v", bad, err)
		}
	}
}
