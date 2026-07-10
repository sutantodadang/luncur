package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Deployment records one deploy. ID and RolledBackFrom are opaque nanoids
// (see NewID), not sequential integers — never parse, sort, or compare them
// for ordering. Seq is the per-app human-facing deploy number (#1, #2, ...);
// it's the only deployment number a user ever sees.
type Deployment struct {
	ID             string
	AppID          int64
	Seq            int64
	Status         string
	ImageRef       string
	LogPath        string
	CreatedBy      sql.NullInt64
	CreatedAt      string
	RolledBackFrom string
}

// maxIDInsertAttempts bounds CreateDeployment/CreateRollbackDeployment's
// retry-on-collision loop: a single retry is already astronomically more
// than the id space's collision odds warrant, but a loop (rather than one
// hardcoded retry) keeps the intent explicit and easy to widen later.
const maxIDInsertAttempts = 2

// isUniqueConstraintErr reports whether err looks like a SQLite UNIQUE
// constraint violation — the only failure mode CreateDeployment/
// CreateRollbackDeployment should retry a fresh id for; any other error
// (e.g. a bad app_id FK) should surface immediately.
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateDeployment inserts a deployment row with a freshly generated
// opaque id, retrying once with a new id on the practically-impossible
// event of a collision. createdBy of 0 is stored as NULL (unattributed).
// seq is assigned atomically as this app's next number (1, 2, 3, ...) via
// a subquery in the same INSERT — safe under modernc sqlite's
// single-writer connection (db.SetMaxOpenConns(1)), no separate
// transaction needed.
func (s *Store) CreateDeployment(appID int64, status, imageRef string, createdBy int64) (Deployment, error) {
	var by any
	if createdBy != 0 {
		by = createdBy
	}
	var lastErr error
	for attempt := 0; attempt < maxIDInsertAttempts; attempt++ {
		id := NewID()
		_, err := s.db.Exec(
			`INSERT INTO deployments (id, app_id, seq, status, image_ref, created_by)
			 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM deployments WHERE app_id = ?), ?, ?, ?)`,
			id, appID, appID, status, imageRef, by,
		)
		if err == nil {
			return s.GetDeployment(id)
		}
		lastErr = err
		if !isUniqueConstraintErr(err) {
			break
		}
	}
	return Deployment{}, fmt.Errorf("insert deployment: %w", lastErr)
}

// CreateRollbackDeployment records a redeploy of an earlier deployment's
// image: status starts at "deploying" (no build phase) and rolled_back_from
// preserves the lineage for history displays.
func (s *Store) CreateRollbackDeployment(appID int64, imageRef string, createdBy int64, rolledBackFrom string) (Deployment, error) {
	var by any
	if createdBy != 0 {
		by = createdBy
	}
	var lastErr error
	for attempt := 0; attempt < maxIDInsertAttempts; attempt++ {
		id := NewID()
		_, err := s.db.Exec(
			`INSERT INTO deployments (id, app_id, seq, status, image_ref, created_by, rolled_back_from)
			 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM deployments WHERE app_id = ?), 'deploying', ?, ?, ?)`,
			id, appID, appID, imageRef, by, rolledBackFrom,
		)
		if err == nil {
			return s.GetDeployment(id)
		}
		lastErr = err
		if !isUniqueConstraintErr(err) {
			break
		}
	}
	return Deployment{}, fmt.Errorf("insert rollback deployment: %w", lastErr)
}

func (s *Store) SetDeploymentStatus(id string, status string) error {
	res, err := s.db.Exec(`UPDATE deployments SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDeploymentImage(id string, imageRef string) error {
	res, err := s.db.Exec(`UPDATE deployments SET image_ref = ? WHERE id = ?`, imageRef, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDeploymentLog(id string, logPath string) error {
	res, err := s.db.Exec(`UPDATE deployments SET log_path = ? WHERE id = ?`, logPath, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetDeployment(id string) (Deployment, error) {
	var d Deployment
	var img, logp, rolledBackFrom sql.NullString
	err := s.db.QueryRow(
		`SELECT id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from
		 FROM deployments WHERE id = ?`, id,
	).Scan(&d.ID, &d.AppID, &d.Seq, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt, &rolledBackFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err == nil {
		d.ImageRef, d.LogPath = img.String, logp.String
		d.RolledBackFrom = rolledBackFrom.String
	}
	return d, err
}

// LatestDeployment returns an app's most recently created deployment.
// Ordered by rowid (SQLite's implicit, monotonically-assigned-on-insert
// column), not id — id is an opaque nanoid with no inherent order now.
func (s *Store) LatestDeployment(appID int64) (Deployment, error) {
	var d Deployment
	var img, logp, rolledBackFrom sql.NullString
	err := s.db.QueryRow(
		`SELECT id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from FROM deployments
		 WHERE app_id = ? ORDER BY rowid DESC LIMIT 1`, appID,
	).Scan(&d.ID, &d.AppID, &d.Seq, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt, &rolledBackFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err == nil {
		d.ImageRef, d.LogPath = img.String, logp.String
		d.RolledBackFrom = rolledBackFrom.String
	}
	return d, err
}

// CountDeployments returns an app's total deploy count (history table cap
// notwithstanding — COUNT is exact).
func (s *Store) CountDeployments(appID int64) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT count(*) FROM deployments WHERE app_id = ?`, appID).Scan(&n)
	return n, err
}

// Ping verifies the database connection is reachable.
func (s *Store) Ping() error {
	var one int
	return s.db.QueryRow(`SELECT 1`).Scan(&one)
}

// StuckDeployments returns deployments still 'building' whose created_at is
// older than olderThanMin minutes, newest first (by rowid — see
// LatestDeployment) — the doctor check's signal that a builder job is stuck
// or the builder image is missing.
func (s *Store) StuckDeployments(olderThanMin int) ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from
		 FROM deployments WHERE status = 'building' AND created_at < datetime('now', ?)
		 ORDER BY rowid DESC`, fmt.Sprintf("-%d minutes", olderThanMin))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var img, logp, rolledBackFrom sql.NullString
		if err := rows.Scan(&d.ID, &d.AppID, &d.Seq, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt, &rolledBackFrom); err != nil {
			return nil, err
		}
		d.ImageRef, d.LogPath = img.String, logp.String
		d.RolledBackFrom = rolledBackFrom.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// UnfinishedDeployments returns every deployment still in 'building' or
// 'deploying', oldest first (by rowid — see LatestDeployment) — the startup
// reconciliation loop's input for resuming or failing deploys orphaned by a
// server restart (the goroutine that was driving them died with the
// process).
func (s *Store) UnfinishedDeployments() ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from
		 FROM deployments WHERE status IN ('building', 'deploying') ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var img, logp, rolledBackFrom sql.NullString
		if err := rows.Scan(&d.ID, &d.AppID, &d.Seq, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt, &rolledBackFrom); err != nil {
			return nil, err
		}
		d.ImageRef, d.LogPath = img.String, logp.String
		d.RolledBackFrom = rolledBackFrom.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDeployments returns an app's deploy history, newest first (by rowid —
// see LatestDeployment).
// ponytail: hard cap 50 — paging when someone actually has 51 deploys to read.
func (s *Store) ListDeployments(appID int64) ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app_id, seq, status, image_ref, log_path, created_by, created_at, rolled_back_from
		 FROM deployments WHERE app_id = ? ORDER BY rowid DESC LIMIT 50`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var img, logp, rolledBackFrom sql.NullString
		if err := rows.Scan(&d.ID, &d.AppID, &d.Seq, &d.Status, &img, &logp, &d.CreatedBy, &d.CreatedAt, &rolledBackFrom); err != nil {
			return nil, err
		}
		d.ImageRef, d.LogPath = img.String, logp.String
		d.RolledBackFrom = rolledBackFrom.String
		out = append(out, d)
	}
	return out, rows.Err()
}
