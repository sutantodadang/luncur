package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHKey is a user's public key for git-push auth.
type SSHKey struct {
	ID          int64
	UserID      int64
	Name        string
	PublicKey   string
	Fingerprint string
	CreatedAt   string
}

// AddSSHKey validates an authorized_keys-format public key and stores it
// with its SHA256 fingerprint. The fingerprint is the auth lookup key, so
// duplicates are rejected by the UNIQUE constraint.
func (s *Store) AddSSHKey(userID int64, name, publicKey string) (SSHKey, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return SSHKey{}, fmt.Errorf("invalid public key: %w", err)
	}
	fp := ssh.FingerprintSHA256(pk)
	norm := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	res, err := s.db.Exec(
		`INSERT INTO ssh_keys (user_id, name, public_key, fingerprint) VALUES (?, ?, ?, ?)`,
		userID, name, norm, fp,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return SSHKey{}, fmt.Errorf("this public key is already registered")
		}
		return SSHKey{}, err
	}
	id, _ := res.LastInsertId()
	return SSHKey{ID: id, UserID: userID, Name: name, PublicKey: norm, Fingerprint: fp}, nil
}

func (s *Store) ListSSHKeys(userID int64) ([]SSHKey, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, public_key, fingerprint, created_at
		 FROM ssh_keys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		var k SSHKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteSSHKey removes one of userID's keys. ErrNotFound covers both a
// missing id and an id owned by someone else, so the API can't leak key
// existence across users.
func (s *Store) DeleteSSHKey(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM ssh_keys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserForSSHFingerprint resolves a public-key fingerprint to its user.
func (s *Store) UserForSSHFingerprint(fp string) (User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT u.id, u.email, u.role FROM ssh_keys k
		 JOIN users u ON u.id = k.user_id WHERE k.fingerprint = ?`, fp,
	).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
