package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Environment is a named, namespace-owning slice of a project: apps and
// addons live inside exactly one. Every project always has at least one
// standing environment (its default, "production" for migrated projects);
// previews are ephemeral environments cloned from a base env per branch.
type Environment struct {
	ID        int64
	ProjectID int64
	Name      string
	Namespace string
	// Kind is "standing" (long-lived, e.g. production/develop/staging) or
	// "preview" (ephemeral, one per git branch).
	Kind      string
	IsDefault bool
	// BaseBranch is the git branch a standing env's apps deploy on push
	// (e.g. "main", "develop"); "" for envs with no webhook-driven deploy.
	BaseBranch string
	// SourceBranch is the git branch a preview env was created from; ""
	// for standing envs.
	SourceBranch string
	LastActiveAt string
	CreatedAt    string
}

// envNamespace derives the Kubernetes namespace for a (project, env) pair.
func envNamespace(project, env string) string {
	return "luncur-" + project + "-" + env
}

const environmentCols = `id, project_id, name, k8s_namespace, kind, is_default, base_branch, source_branch, last_active_at, created_at`

func scanEnvironmentRow(row *sql.Row) (Environment, error) {
	var e Environment
	var isDefault int
	err := row.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Namespace, &e.Kind, &isDefault, &e.BaseBranch, &e.SourceBranch, &e.LastActiveAt, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, err
	}
	e.IsDefault = isDefault != 0
	return e, nil
}

// CreateEnvironment creates a new environment under projectID. kind must be
// "standing" or "preview" ("" normalizes to "standing"). The namespace is
// derived from the project's name, not caller-supplied.
func (s *Store) CreateEnvironment(projectID int64, name, kind, baseBranch string) (Environment, error) {
	if !validName(name) {
		return Environment{}, validationErrorf("invalid environment name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if kind == "" {
		kind = "standing"
	}
	if kind != "standing" && kind != "preview" {
		return Environment{}, validationErrorf("invalid environment kind %q (standing|preview)", kind)
	}
	p, err := s.GetProjectByID(projectID)
	if err != nil {
		return Environment{}, err
	}
	ns := envNamespace(p.Name, name)
	res, err := s.db.Exec(
		`INSERT INTO environments (project_id, name, k8s_namespace, kind, base_branch) VALUES (?, ?, ?, ?, ?)`,
		projectID, name, ns, kind, baseBranch,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Environment{}, validationErrorf("environment %q already exists", name)
		}
		return Environment{}, fmt.Errorf("insert environment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Environment{}, err
	}
	return s.GetEnvironmentByID(id)
}

// SeedProjectEnvironments creates a fresh project's standard 3 environments
// (production/develop/staging) and sets its default_env/preview_base_env,
// via the exact same core backfillEnvironments uses for legacy projects at
// migration time: production reuses the project's own k8s_namespace (so a
// brand new project's default-environment behavior is byte-identical to the
// pre-environments behavior — no "-production" suffix), develop/staging get
// their own namespaces. Idempotent in effect: a project that already has
// environments is left untouched (mirrors backfillEnvironments' guard).
func (s *Store) SeedProjectEnvironments(projectID int64) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM environments WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	p, err := s.GetProjectByID(projectID)
	if err != nil {
		return err
	}
	return backfillProjectEnvironments(s.db, p.ID, p.Name, p.Namespace)
}

// GetEnvironment looks up an environment by its project-scoped name.
func (s *Store) GetEnvironment(projectID int64, name string) (Environment, error) {
	return scanEnvironmentRow(s.db.QueryRow(
		`SELECT `+environmentCols+` FROM environments WHERE project_id = ? AND name = ?`, projectID, name))
}

// GetEnvironmentByID looks up an environment by its primary key, for code
// that only has a foreign key reference (e.g. an App row).
func (s *Store) GetEnvironmentByID(id int64) (Environment, error) {
	return scanEnvironmentRow(s.db.QueryRow(
		`SELECT `+environmentCols+` FROM environments WHERE id = ?`, id))
}

// ListEnvironments returns every environment for a project, ordered by name.
func (s *Store) ListEnvironments(projectID int64) ([]Environment, error) {
	rows, err := s.db.Query(`SELECT `+environmentCols+` FROM environments WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		var isDefault int
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Namespace, &e.Kind, &isDefault, &e.BaseBranch, &e.SourceBranch, &e.LastActiveAt, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.IsDefault = isDefault != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEnvironment removes the environment row. The caller is responsible
// for kube namespace teardown first (mirrors DeleteProject/DeleteApp).
func (s *Store) DeleteEnvironment(id int64) error {
	res, err := s.db.Exec(`DELETE FROM environments WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDefaultEnvironment marks envID as projectID's default environment,
// clearing any previous default in the same transaction so a caller never
// observes zero or multiple defaults.
func (s *Store) SetDefaultEnvironment(projectID, envID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if _, err := tx.Exec(`UPDATE environments SET is_default = 0 WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("clear existing default: %w", err)
	}
	res, err := tx.Exec(`UPDATE environments SET is_default = 1 WHERE id = ? AND project_id = ?`, envID, projectID)
	if err != nil {
		return fmt.Errorf("set new default: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// TouchEnvironment bumps last_active_at to now. Called on every deploy so a
// later idle-TTL reaper only tears down truly-idle preview environments.
func (s *Store) TouchEnvironment(id int64) error {
	res, err := s.db.Exec(`UPDATE environments SET last_active_at = datetime('now') WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnvironmentSourceBranch sets the git branch a preview environment was
// created from (Environment.SourceBranch). Only meaningful for kind
// 'preview' rows; the server layer (ensurePreview) enforces that — the
// store only persists the value.
func (s *Store) SetEnvironmentSourceBranch(id int64, branch string) error {
	res, err := s.db.Exec(`UPDATE environments SET source_branch = ? WHERE id = ?`, branch, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
