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
	// GPUQuota caps the total nvidia.com/gpu devices the project's apps may
	// request. 0 means unlimited (today's behavior).
	GPUQuota int64
	// CPUQuotaMilli and MemQuotaMB cap the project namespace's total
	// limits.cpu/limits.memory via a ResourceQuota. 0 means unlimited;
	// enforced by a namespace ResourceQuota on limits.cpu/limits.memory.
	CPUQuotaMilli, MemQuotaMB int64
	// DefaultEnv is the environment name legacy (env-less) routes and CLI
	// calls resolve to. "production" at the SQL level for a bare row;
	// project creation (server layer) seeds the 3 standard envs and confirms
	// this explicitly.
	DefaultEnv string
	// PreviewBaseEnv is the environment new preview environments clone their
	// app specs and addon data from. "develop" at the SQL level for a bare
	// row.
	PreviewBaseEnv string
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

// RenameProject changes a project's display/API name. The Kubernetes
// namespace is derived at creation and never renamed — existing cluster
// objects stay where they are.
func (s *Store) RenameProject(id int64, name string) error {
	if !validName(name) {
		return validationErrorf("invalid project name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	res, err := s.db.Exec(`UPDATE projects SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("rename project: %w", err)
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

// DeleteProject removes the project row and its memberships. App and addon
// rows are the caller's job (destroyApp/removeAddon per row) so kube
// teardown can't be skipped by a bare row delete.
func (s *Store) DeleteProject(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM project_members WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("delete project members: %w", err)
	}
	res, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
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

func (s *Store) GetProject(name string) (Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, k8s_namespace, gpu_quota, cpu_quota_milli, mem_quota_mb, default_env, preview_base_env FROM projects WHERE name = ?`, name,
	).Scan(&p.ID, &p.Name, &p.Namespace, &p.GPUQuota, &p.CPUQuotaMilli, &p.MemQuotaMB, &p.DefaultEnv, &p.PreviewBaseEnv)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// GetProjectByID looks up a project by its primary key, for code that only
// has a foreign key reference (e.g. an App row) and not the project's name.
func (s *Store) GetProjectByID(id int64) (Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, k8s_namespace, gpu_quota, cpu_quota_milli, mem_quota_mb, default_env, preview_base_env FROM projects WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &p.Namespace, &p.GPUQuota, &p.CPUQuotaMilli, &p.MemQuotaMB, &p.DefaultEnv, &p.PreviewBaseEnv)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id, name, k8s_namespace, gpu_quota, cpu_quota_milli, mem_quota_mb, default_env, preview_base_env FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Namespace, &p.GPUQuota, &p.CPUQuotaMilli, &p.MemQuotaMB, &p.DefaultEnv, &p.PreviewBaseEnv); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProjectGPUQuota sets the project's GPU budget. 0 means unlimited.
func (s *Store) SetProjectGPUQuota(projectID, quota int64) error {
	if quota < 0 {
		return fmt.Errorf("gpu quota must be >= 0")
	}
	res, err := s.db.Exec(`UPDATE projects SET gpu_quota = ? WHERE id = ?`, quota, projectID)
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

// SetProjectResourceQuota sets the project's namespace CPU/memory budget.
// 0 means unlimited for either.
func (s *Store) SetProjectResourceQuota(projectID, cpuMilli, memMB int64) error {
	if cpuMilli < 0 || memMB < 0 {
		return fmt.Errorf("cpu and memory quota must be >= 0")
	}
	res, err := s.db.Exec(`UPDATE projects SET cpu_quota_milli = ?, mem_quota_mb = ? WHERE id = ?`, cpuMilli, memMB, projectID)
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

// SetDefaultEnv sets the environment name legacy (env-less) routes and CLI
// calls resolve to. The caller (server layer) is responsible for confirming
// env exists; the store only validates the name shape.
func (s *Store) SetDefaultEnv(projectID int64, env string) error {
	if !validName(env) {
		return validationErrorf("invalid environment name %q (lowercase letters, digits, dashes; max 40 chars)", env)
	}
	res, err := s.db.Exec(`UPDATE projects SET default_env = ? WHERE id = ?`, env, projectID)
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

// SetPreviewBaseEnv sets the environment new preview environments clone
// their app specs and addon data from. The caller (server layer) is
// responsible for confirming env exists; the store only validates the name
// shape.
func (s *Store) SetPreviewBaseEnv(projectID int64, env string) error {
	if !validName(env) {
		return validationErrorf("invalid environment name %q (lowercase letters, digits, dashes; max 40 chars)", env)
	}
	res, err := s.db.Exec(`UPDATE projects SET preview_base_env = ? WHERE id = ?`, env, projectID)
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

// SumProjectGPURequests totals the GPUs the project's apps would request if
// all ran at once: gpu × replicas for web/worker, gpu × 1 for cron, gpu ×
// nodes for job (a job app's planned footprint is its multi-node run shape
// — see App.Nodes — not its largely-unused replicas column).
// UX estimate only — the namespace ResourceQuota is the hard enforcement.
func (s *Store) SumProjectGPURequests(projectID int64) (int64, error) {
	var sum int64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(gpu_count * CASE
			WHEN kind = 'cron' THEN 1
			WHEN kind = 'job' THEN MAX(nodes, 1)
			ELSE MAX(replicas, 1)
		END), 0)
		 FROM apps WHERE project_id = ?`, projectID).Scan(&sum)
	return sum, err
}
