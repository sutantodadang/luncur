package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type App struct {
	ID        int64
	ProjectID int64
	Name      string
	Port      int
	Replicas  int
}

func (s *Store) CreateApp(projectID int64, name string, port int) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if port < 1 || port > 65535 {
		return App{}, fmt.Errorf("invalid port %d", port)
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, port) VALUES (?, ?, 'tarball', ?)`,
		projectID, name, port,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{ID: id, ProjectID: projectID, Name: name, Port: port, Replicas: 1}, nil
}

func (s *Store) GetApp(projectID int64, name string) (App, error) {
	var a App
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas FROM apps WHERE project_id = ? AND name = ?`,
		projectID, name,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func (s *Store) ListApps(projectID int64) ([]App, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, name, port, replicas FROM apps WHERE project_id = ? ORDER BY name`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteApp(id int64) error {
	res, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetReplicas(id int64, n int) error {
	if n < 0 || n > 20 {
		return fmt.Errorf("replicas must be 0-20, got %d", n)
	}
	res, err := s.db.Exec(`UPDATE apps SET replicas = ? WHERE id = ?`, n, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
