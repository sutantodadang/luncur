package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

var ErrAuthFailed = errors.New("authentication failed")

// dummyHash is compared against on unknown-email logins so that the bcrypt
// work factor is paid regardless of whether the email exists. This equalizes
// timing between "unknown email" and "wrong password" responses so a caller
// can't use response latency to enumerate valid emails.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("luncur-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}()

// ValidationError signals bad caller input (as opposed to a store/db
// failure) so HTTP handlers can map it to a 400 with a clean message.
type ValidationError struct {
	msg string
}

func (e *ValidationError) Error() string { return e.msg }

func validationErrorf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

type User struct {
	ID    int64
	Email string
	Role  string
}

func (s *Store) CreateUser(email, password, role string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return User{}, validationErrorf("email required")
	}
	if role != "admin" && role != "member" {
		return User{}, validationErrorf("invalid role %q", role)
	}
	if len(password) < 8 {
		return User{}, validationErrorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO users (email, password_hash, role) VALUES (?, ?, ?)`,
		email, string(hash), role,
	)
	if err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, Role: role}, nil
}

func (s *Store) Authenticate(email, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var u User
	var hash string
	err := s.db.QueryRow(
		`SELECT id, email, role, password_hash FROM users WHERE email = ?`, email,
	).Scan(&u.ID, &u.Email, &u.Role, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Burn the same bcrypt cost as a real comparison so unknown-email
		// and wrong-password responses take equal time.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return User{}, ErrAuthFailed
	}
	return u, nil
}

// UserInfo is the admin listing view of a user.
type UserInfo struct {
	ID         int64
	Email      string
	Role       string
	CreatedAt  string
	TokenCount int64
}

// ListUsers returns every user with their live token count.
func (s *Store) ListUsers() ([]UserInfo, error) {
	rows, err := s.db.Query(
		`SELECT u.id, u.email, u.role, u.created_at, COUNT(t.id)
		 FROM users u LEFT JOIN api_tokens t ON t.user_id = u.id
		 GROUP BY u.id ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserInfo
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &u.TokenCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdatePassword rehashes and stores a user's password. Same length rule as
// CreateUser; ErrNotFound for an unknown id.
func (s *Store) UpdatePassword(id int64, password string) error {
	if len(password) < 8 {
		return validationErrorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateEmail changes a user's login email (normalized like CreateUser).
// ErrNotFound for an unknown id; the users.email UNIQUE constraint surfaces
// as an error the caller maps to a conflict.
func (s *Store) UpdateEmail(id int64, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return validationErrorf("email required")
	}
	res, err := s.db.Exec(`UPDATE users SET email = ? WHERE id = ?`, email, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user; tokens, ssh keys, and memberships cascade.
func (s *Store) DeleteUser(id int64) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
