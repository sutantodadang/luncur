package store

import (
	"database/sql"
	"errors"
)

// JobRun is one triggered run of a kind=job app. ExitCode is only valid
// once the run has finished, and may stay invalid when the pod's exit code
// could not be determined.
type JobRun struct {
	ID         int64
	AppID      int64
	Status     string // running|succeeded|failed
	Nodes      int
	Framework  string
	ExitCode   sql.NullInt64
	StartedAt  string
	FinishedAt sql.NullString
}

// CreateJobRun records the start of a run (status "running") with the node
// count and framework preset the run actually uses.
func (s *Store) CreateJobRun(appID int64, nodes int, framework string) (JobRun, error) {
	if nodes < 1 {
		nodes = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO job_runs (app_id, status, nodes, framework) VALUES (?, 'running', ?, ?)`,
		appID, nodes, framework)
	if err != nil {
		return JobRun{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return JobRun{}, err
	}
	return s.GetJobRun(id)
}

const jobRunCols = `id, app_id, status, nodes, framework, exit_code, started_at, finished_at`

func scanJobRun(row *sql.Row) (JobRun, error) {
	var r JobRun
	err := row.Scan(&r.ID, &r.AppID, &r.Status, &r.Nodes, &r.Framework, &r.ExitCode, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return JobRun{}, ErrNotFound
	}
	return r, err
}

func (s *Store) GetJobRun(id int64) (JobRun, error) {
	return scanJobRun(s.db.QueryRow(`SELECT `+jobRunCols+` FROM job_runs WHERE id = ?`, id))
}

// FinishJobRun marks a run finished. exitCode may be nil (unknown).
func (s *Store) FinishJobRun(id int64, status string, exitCode *int64) error {
	if status != "succeeded" && status != "failed" {
		return errors.New("finish status must be succeeded or failed")
	}
	var code any
	if exitCode != nil {
		code = *exitCode
	}
	res, err := s.db.Exec(
		`UPDATE job_runs SET status = ?, exit_code = ?, finished_at = datetime('now') WHERE id = ?`,
		status, code, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListJobRuns returns an app's runs, newest first, capped at 50.
func (s *Store) ListJobRuns(appID int64) ([]JobRun, error) {
	rows, err := s.db.Query(
		`SELECT `+jobRunCols+` FROM job_runs WHERE app_id = ? ORDER BY id DESC LIMIT 50`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRun
	for rows.Next() {
		var r JobRun
		if err := rows.Scan(&r.ID, &r.AppID, &r.Status, &r.Nodes, &r.Framework, &r.ExitCode, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
