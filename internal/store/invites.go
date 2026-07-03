package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// Invite is a single-use, role-carrying registration token.
type Invite struct {
	Token     string
	Role      string
	ExpiresAt string
	CreatedBy int64
	UsedBy    int64
	UsedAt    string
}

// CreateInvite mints a 7-day, single-use invite.
func (s *Store) CreateInvite(role string, createdBy int64) (Invite, error) {
	if role != "admin" && role != "member" {
		return Invite{}, fmt.Errorf("invalid role %q", role)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Invite{}, err
	}
	token := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO invites (token, role, expires_at, created_by)
		 VALUES (?, ?, datetime('now', '+7 days'), ?)`, token, role, createdBy)
	if err != nil {
		return Invite{}, err
	}
	var inv Invite
	err = s.db.QueryRow(
		`SELECT token, role, expires_at FROM invites WHERE token = ?`, token,
	).Scan(&inv.Token, &inv.Role, &inv.ExpiresAt)
	inv.CreatedBy = createdBy
	return inv, err
}

const inviteCols = `token, role, expires_at, COALESCE(created_by, 0), COALESCE(used_by, 0), COALESCE(used_at, '')`

func (s *Store) scanInvites(query string, args ...any) ([]Invite, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.Token, &i.Role, &i.ExpiresAt, &i.CreatedBy, &i.UsedBy, &i.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ListInvites returns every invite, newest first.
func (s *Store) ListInvites() ([]Invite, error) {
	return s.scanInvites(`SELECT ` + inviteCols + ` FROM invites ORDER BY rowid DESC`)
}

// GetValidInvite returns an invite iff it exists, is unused, and unexpired.
func (s *Store) GetValidInvite(token string) (Invite, error) {
	var i Invite
	err := s.db.QueryRow(
		`SELECT `+inviteCols+` FROM invites
		 WHERE token = ? AND used_by IS NULL AND expires_at > datetime('now')`, token,
	).Scan(&i.Token, &i.Role, &i.ExpiresAt, &i.CreatedBy, &i.UsedBy, &i.UsedAt)
	if err == sql.ErrNoRows {
		return Invite{}, ErrNotFound
	}
	return i, err
}

// MarkInviteUsed burns the invite; the WHERE guard makes double-use lose.
func (s *Store) MarkInviteUsed(token string, userID int64) error {
	res, err := s.db.Exec(
		`UPDATE invites SET used_by = ?, used_at = datetime('now')
		 WHERE token = ? AND used_by IS NULL AND expires_at > datetime('now')`,
		userID, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RevokeInvite(token string) error {
	res, err := s.db.Exec(`DELETE FROM invites WHERE token = ?`, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
