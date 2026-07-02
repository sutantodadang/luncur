package store

import (
	"database/sql"
	"errors"
	"strings"
)

// GetUserByEmail looks up a user by email (case-insensitive, matching
// CreateUser's normalization). Returns ErrNotFound when absent.
func (s *Store) GetUserByEmail(email string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var u User
	err := s.db.QueryRow(
		`SELECT id, email, role FROM users WHERE email = ?`, email,
	).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// AddMember adds userID to projectID's membership as 'member'. Idempotent:
// re-adding an existing member is a no-op.
func (s *Store) AddMember(projectID, userID int64) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO project_members (project_id, user_id, role) VALUES (?, ?, 'member')`,
		projectID, userID,
	)
	return err
}

func (s *Store) IsMember(projectID, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	).Scan(&n)
	return n > 0, err
}

// ListProjectsFor returns the projects userID is a member of.
func (s *Store) ListProjectsFor(userID int64) ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT p.id, p.name, p.k8s_namespace FROM projects p
		 JOIN project_members m ON m.project_id = p.id WHERE m.user_id = ?
		 ORDER BY p.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Namespace); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
