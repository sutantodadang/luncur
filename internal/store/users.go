package store

import (
	"database/sql"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

var ErrAuthFailed = errors.New("authentication failed")

type User struct {
	ID    int64
	Email string
	Role  string
}

func (s *Store) CreateUser(email, password, role string) (User, error) {
	if email == "" {
		return User{}, errors.New("email required")
	}
	if role != "admin" && role != "member" {
		return User{}, fmt.Errorf("invalid role %q", role)
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
	var u User
	var hash string
	err := s.db.QueryRow(
		`SELECT id, email, role, password_hash FROM users WHERE email = ?`, email,
	).Scan(&u.ID, &u.Email, &u.Role, &hash)
	if errors.Is(err, sql.ErrNoRows) {
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
