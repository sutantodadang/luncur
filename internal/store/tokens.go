package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// CreateToken mints a 90-day API token (CLI/API use). The plaintext is
// returned exactly once; the DB keeps only its SHA-256.
func (s *Store) CreateToken(userID int64, name string) (string, error) {
	return s.createToken(userID, name, "+90 days")
}

// CreateSessionToken mints a short-lived token backing a web session; its
// server-side expiry matches the session cookie's 7-day lifetime.
func (s *Store) CreateSessionToken(userID int64, name string) (string, error) {
	return s.createToken(userID, name, "+7 days")
}

func (s *Store) createToken(userID int64, name, offset string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plaintext := "lcr_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	_, err := s.db.Exec(
		`INSERT INTO api_tokens (user_id, hash, name, expires_at)
		 VALUES (?, ?, ?, datetime('now', ?))`,
		userID, hex.EncodeToString(sum[:]), name, offset,
	)
	if err != nil {
		return "", fmt.Errorf("insert token: %w", err)
	}
	return plaintext, nil
}

func (s *Store) UserForToken(plaintext string) (User, error) {
	sum := sha256.Sum256([]byte(plaintext))
	h := hex.EncodeToString(sum[:])
	var u User
	err := s.db.QueryRow(
		`SELECT u.id, u.email, u.role FROM api_tokens t
		 JOIN users u ON u.id = t.user_id
		 WHERE t.hash = ? AND (t.expires_at IS NULL OR t.expires_at > datetime('now'))`, h,
	).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	// Best-effort: a failed timestamp update must never fail authentication.
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at = datetime('now') WHERE hash = ?`, h)
	return u, nil
}

// TokenInfo is the listing view of an API token — never the hash.
type TokenInfo struct {
	ID         int64
	Name       string
	CreatedAt  string
	LastUsedAt string
	ExpiresAt  string
}

// ListTokens returns a user's tokens, newest first.
func (s *Store) ListTokens(userID int64) ([]TokenInfo, error) {
	rows, err := s.db.Query(
		`SELECT id, name, created_at, COALESCE(last_used_at, ''), COALESCE(expires_at, '')
		 FROM api_tokens WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken deletes one of userID's tokens. ErrNotFound covers both a
// missing id and someone else's token.
func (s *Store) RevokeToken(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
