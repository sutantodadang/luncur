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

// AddMember adds userID to projectID's membership with the given role
// ("member" or "viewer"). Idempotent: re-adding an existing member updates
// their role.
func (s *Store) AddMember(projectID, userID int64, role string) error {
	if role != "member" && role != "viewer" {
		return validationErrorf("invalid role %q (must be member or viewer)", role)
	}
	_, err := s.db.Exec(
		`INSERT INTO project_members (project_id, user_id, role) VALUES (?, ?, ?)
		 ON CONFLICT (project_id, user_id) DO UPDATE SET role = excluded.role`,
		projectID, userID, role,
	)
	return err
}

// MemberRole returns userID's role in projectID ("member"/"viewer"), or
// ErrNotFound when they are not a member.
func (s *Store) MemberRole(projectID, userID int64) (string, error) {
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// RemoveMember drops userID from projectID's membership. ErrNotFound when
// the membership didn't exist.
func (s *Store) RemoveMember(projectID, userID int64) error {
	res, err := s.db.Exec(
		`DELETE FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) IsMember(projectID, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID,
	).Scan(&n)
	return n > 0, err
}

// ListMembers returns projectID's members (email + role), ordered by email.
func (s *Store) ListMembers(projectID int64) ([]User, error) {
	rows, err := s.db.Query(
		`SELECT u.id, u.email, u.role FROM users u
		 JOIN project_members m ON m.user_id = u.id WHERE m.project_id = ?
		 ORDER BY u.email`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
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
