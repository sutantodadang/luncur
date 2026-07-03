package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type App struct {
	ID         int64
	ProjectID  int64
	Name       string
	Port       int
	Replicas   int
	SourceType string
	GitURL     string
	GitBranch  string
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
	return App{ID: id, ProjectID: projectID, Name: name, Port: port, Replicas: 1, SourceType: "tarball"}, nil
}

// CreateGitApp registers an app whose source is a git repo cloned at deploy
// time. Phase 1 supports public repos / token-in-URL only; the git_token_enc
// column stays unused until private-repo token sealing lands.
func (s *Store) CreateGitApp(projectID int64, name string, port int, gitURL, gitBranch string) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if port < 1 || port > 65535 {
		return App{}, fmt.Errorf("invalid port %d", port)
	}
	if gitURL == "" {
		return App{}, fmt.Errorf("git url is required")
	}
	if gitBranch == "" {
		gitBranch = "main"
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, git_url, git_branch, port) VALUES (?, ?, 'git', ?, ?, ?)`,
		projectID, name, gitURL, gitBranch, port,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{
		ID: id, ProjectID: projectID, Name: name, Port: port, Replicas: 1,
		SourceType: "git", GitURL: gitURL, GitBranch: gitBranch,
	}, nil
}

func (s *Store) GetApp(projectID int64, name string) (App, error) {
	var a App
	var gitURL, gitBranch sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch FROM apps WHERE project_id = ? AND name = ?`,
		projectID, name,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.GitURL = gitURL.String
	a.GitBranch = gitBranch.String
	return a, nil
}

// GetAppByID looks up an app by its primary key, for code that only has a
// foreign key reference (e.g. a Domain row) and not the owning project's
// name.
func (s *Store) GetAppByID(id int64) (App, error) {
	var a App
	var gitURL, gitBranch sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch FROM apps WHERE id = ?`,
		id,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.GitURL = gitURL.String
	a.GitBranch = gitBranch.String
	return a, nil
}

func (s *Store) ListApps(projectID int64) ([]App, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch FROM apps WHERE project_id = ? ORDER BY name`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var gitURL, gitBranch sql.NullString
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch); err != nil {
			return nil, err
		}
		a.GitURL = gitURL.String
		a.GitBranch = gitBranch.String
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
