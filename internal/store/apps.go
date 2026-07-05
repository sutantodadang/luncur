package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	Ejected    bool
	CPUMilli   int64
	MemoryMB   int64
	HealthPath string
	// Kind is one of web|worker|cron; "" normalizes to "web" on create.
	Kind string
	// Schedule is a 5-field cron expression; only set (and required) for
	// cron apps.
	Schedule string
	// WebhookSecret is the sealed (AES-256-GCM) webhook secret; nil means
	// the webhook is disabled. Only meaningful for git-source apps.
	WebhookSecret []byte
	// BuildPath is an optional repo-relative subdirectory used as the build
	// context/detection dir, letting one git repo back several apps
	// (monorepo support). "" (the default) builds the repo root — the
	// pre-existing behavior. Set at create time; immutable thereafter
	// (recreate the app to change it).
	BuildPath string
	// Internal marks a web app as cluster-only: render emits a ClusterIP
	// Service but no Ingress, so the app has no public URL — only reachable
	// from other apps in the same cluster. Only meaningful for kind "web";
	// worker/cron already have no Service to make internal.
	Internal bool
}

// normalizeAppKind defaults an empty kind to "web" for back-compat with
// callers written before app kinds existed.
func normalizeAppKind(kind string) string {
	if kind == "" {
		return "web"
	}
	return kind
}

// validateAppKind enforces the port/schedule matrix per app kind: web needs
// a port and no schedule; worker needs neither; cron needs a validated
// schedule and no port.
func validateAppKind(kind string, port int, schedule string) error {
	switch kind {
	case "web":
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid port %d", port)
		}
		if schedule != "" {
			return fmt.Errorf("schedule is only valid for cron apps")
		}
	case "worker":
		if port != 0 {
			return fmt.Errorf("worker apps do not take a port")
		}
		if schedule != "" {
			return fmt.Errorf("schedule is only valid for cron apps")
		}
	case "cron":
		if port != 0 {
			return fmt.Errorf("cron apps do not take a port")
		}
		if schedule == "" {
			return fmt.Errorf("cron apps require a schedule")
		}
		if err := validCronSpec(schedule); err != nil {
			return fmt.Errorf("invalid schedule: %w", err)
		}
	default:
		return fmt.Errorf("invalid kind %q (want web, worker, or cron)", kind)
	}
	return nil
}

func (s *Store) CreateApp(projectID int64, name string, port int, kind, schedule string) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	kind = normalizeAppKind(kind)
	if err := validateAppKind(kind, port, schedule); err != nil {
		return App{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, port, kind, schedule) VALUES (?, ?, 'tarball', ?, ?, ?)`,
		projectID, name, port, kind, schedule,
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
		SourceType: "tarball", Kind: kind, Schedule: schedule,
	}, nil
}

// CreateGitApp registers an app whose source is a git repo cloned at deploy
// time. Phase 1 supports public repos / token-in-URL only; the git_token_enc
// column stays unused until private-repo token sealing lands.
func (s *Store) CreateGitApp(projectID int64, name string, port int, gitURL, gitBranch, kind, schedule string) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	kind = normalizeAppKind(kind)
	if err := validateAppKind(kind, port, schedule); err != nil {
		return App{}, err
	}
	if gitURL == "" {
		return App{}, fmt.Errorf("git url is required")
	}
	if gitBranch == "" {
		gitBranch = "main"
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, git_url, git_branch, port, kind, schedule) VALUES (?, ?, 'git', ?, ?, ?, ?, ?)`,
		projectID, name, gitURL, gitBranch, port, kind, schedule,
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
		SourceType: "git", GitURL: gitURL, GitBranch: gitBranch, Kind: kind, Schedule: schedule,
	}, nil
}

func (s *Store) GetApp(projectID int64, name string) (App, error) {
	var a App
	var gitURL, gitBranch sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal FROM apps WHERE project_id = ? AND name = ?`,
		projectID, name,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal)
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
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal FROM apps WHERE id = ?`,
		id,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal)
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
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal FROM apps WHERE project_id = ? ORDER BY name`,
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
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal); err != nil {
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

// SetAppEjected marks an app as ejected from luncur's management.
// SetAppAdopted reverses it.
func (s *Store) SetAppEjected(id int64) error {
	res, err := s.db.Exec(`UPDATE apps SET ejected = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppAdopted clears the ejected flag: luncur manages the app again.
func (s *Store) SetAppAdopted(id int64) error {
	res, err := s.db.Exec(`UPDATE apps SET ejected = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
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

// SetResources sets the app's CPU (milli) and memory (MiB) request+limit.
// 0 clears (no resources rendered).
func (s *Store) SetResources(id int64, cpuMilli, memoryMB int64) error {
	if cpuMilli < 0 || memoryMB < 0 {
		return fmt.Errorf("cpu/memory must be >= 0, got cpu=%d memory=%d", cpuMilli, memoryMB)
	}
	res, err := s.db.Exec(`UPDATE apps SET cpu_milli = ?, memory_mb = ? WHERE id = ?`, cpuMilli, memoryMB, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetWebhookSecret sets (or, with nil/empty, clears) the app's sealed
// webhook secret. nil/empty stores NULL (webhook disabled).
func (s *Store) SetWebhookSecret(id int64, sealed []byte) error {
	var v any
	if len(sealed) > 0 {
		v = sealed
	}
	res, err := s.db.Exec(`UPDATE apps SET webhook_secret = ? WHERE id = ?`, v, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetHealthPath sets the HTTP path probed for readiness/liveness.
// "" clears (no probes rendered).
func (s *Store) SetHealthPath(id int64, path string) error {
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("health path must start with '/', got %q", path)
		}
		if len(path) > 256 {
			return fmt.Errorf("health path must be at most 256 characters, got %d", len(path))
		}
		for _, r := range path {
			if r <= ' ' || r == 0x7f {
				return fmt.Errorf("health path must not contain whitespace or control characters")
			}
		}
	}
	res, err := s.db.Exec(`UPDATE apps SET health_path = ? WHERE id = ?`, path, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetBuildPath sets the app's build_path (repo-relative subdirectory used as
// the build context/detection dir). Format validation happens at the
// API/UI layer (server.validBuildPath) before this is called; this just
// persists the already-validated value.
func (s *Store) SetBuildPath(id int64, path string) error {
	res, err := s.db.Exec(`UPDATE apps SET build_path = ? WHERE id = ?`, path, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInternal sets the app's internal flag (ClusterIP Service, no Ingress,
// no public URL). Kind/internal validation happens at the API/UI layer
// (server.validateInternalKind) before this is called; this just persists
// the already-validated value.
func (s *Store) SetInternal(id int64, internal bool) error {
	res, err := s.db.Exec(`UPDATE apps SET internal = ? WHERE id = ?`, internal, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
