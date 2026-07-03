package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

var ErrNotFound = errors.New("not found")

// validName enforces a DNS-1123 label (1-40 chars) so names can become
// Kubernetes namespaces, object names, and hostnames unmodified.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

func validName(s string) bool { return nameRe.MatchString(s) }

type Project struct {
	ID        int64
	Name      string
	Namespace string
}

func (s *Store) CreateProject(name string) (Project, error) {
	if !validName(name) {
		return Project{}, validationErrorf("invalid project name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	ns := "luncur-" + name
	res, err := s.db.Exec(`INSERT INTO projects (name, k8s_namespace) VALUES (?, ?)`, name, ns)
	if err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Namespace: ns}, nil
}

func (s *Store) GetProject(name string) (Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, k8s_namespace FROM projects WHERE name = ?`, name,
	).Scan(&p.ID, &p.Name, &p.Namespace)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id, name, k8s_namespace FROM projects ORDER BY name`)
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
