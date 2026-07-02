package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// CreateToken mints an opaque API token for a user. The plaintext is
// returned exactly once; the DB keeps only its SHA-256.
func (s *Store) CreateToken(userID int64, name string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plaintext := "lcr_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	_, err := s.db.Exec(
		`INSERT INTO api_tokens (user_id, hash, name) VALUES (?, ?, ?)`,
		userID, hex.EncodeToString(sum[:]), name,
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
		 JOIN users u ON u.id = t.user_id WHERE t.hash = ?`, h,
	).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	s.db.Exec(`UPDATE api_tokens SET last_used_at = datetime('now') WHERE hash = ?`, h)
	return u, nil
}
