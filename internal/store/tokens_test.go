package store

import (
	"errors"
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("t@b.co", "pw123456", "member")
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

func TestExpiredTokenRejected(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("tok@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh token authenticates and has an expiry ~90 days out.
	if _, err := s.UserForToken(tok); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	var exp string
	if err := s.DB().QueryRow(`SELECT expires_at FROM api_tokens WHERE name = 'test'`).Scan(&exp); err != nil {
		t.Fatalf("expires_at not set: %v", err)
	}
	// Force it into the past; auth must now fail.
	if _, err := s.DB().Exec(`UPDATE api_tokens SET expires_at = datetime('now', '-1 day') WHERE name = 'test'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForToken(tok); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expired token: got %v, want ErrAuthFailed", err)
	}
}

func TestSessionTokenExpiresInSevenDays(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("sess@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateSessionToken(u.ID, "session-exp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForToken(tok); err != nil {
		t.Fatalf("fresh session token rejected: %v", err)
	}
	// Server-side expiry must land ~7 days out, not the API token's 90.
	var ok bool
	if err := s.DB().QueryRow(
		`SELECT expires_at > datetime('now', '+6 days')
		    AND expires_at < datetime('now', '+8 days')
		 FROM api_tokens WHERE name = 'session-exp'`,
	).Scan(&ok); err != nil {
		t.Fatalf("expires_at lookup: %v", err)
	}
	if !ok {
		var exp string
		_ = s.DB().QueryRow(`SELECT expires_at FROM api_tokens WHERE name = 'session-exp'`).Scan(&exp)
		t.Fatalf("session token expires_at = %q, want ~7 days out", exp)
	}
}

func TestListAndRevokeTokens(t *testing.T) {
	st := openTest(t)
	u, _ := st.CreateUser("tok2@example.com", "password123", "member")
	other, _ := st.CreateUser("other@example.com", "password123", "member")
	if _, err := st.CreateToken(u.ID, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToken(u.ID, "ci"); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListTokens(u.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].Name != "ci" { // newest first
		t.Fatalf("order: %+v", list)
	}
	if list[0].ExpiresAt == "" {
		t.Fatal("expires_at missing")
	}
	// Foreign revoke → ErrNotFound; own revoke works and kills auth.
	if err := st.RevokeToken(other.ID, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign revoke: %v", err)
	}
	if err := st.RevokeToken(u.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.ListTokens(u.ID); len(l) != 1 {
		t.Fatalf("after revoke: %+v", l)
	}
}
