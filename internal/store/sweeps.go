package store

import (
	"database/sql"
	"errors"
)

// Sweep is a hyperparameter search over a kind=job app: MaxTrials trial
// configurations are pre-expanded at creation (grid or random, see
// internal/sweep) and driven Parallel-at-a-time by the server's sweep loop.
type Sweep struct {
	ID        string
	AppID     int64
	Metric    string
	Direction string // "min" | "max"
	MaxTrials int
	Parallel  int
	EarlyStop bool
	Nodes     int
	Framework string
	Seed      int64
	Status    string // running|done|stopped|failed
	Warning   string // sticky operator-facing note (e.g. mlflow degraded)
	CreatedBy sql.NullInt64
	CreatedAt string
}

// SweepTrial is one configuration within a Sweep. RunID is set once the
// trial has been launched as a job_runs row.
type SweepTrial struct {
	ID          string
	SweepID     string
	RunID       sql.NullInt64 // job_runs.id once launched
	ParamsJSON  string        // map[string]string
	MetricValue sql.NullFloat64
	MetricStep  sql.NullInt64
	State       string // pending|running|done|failed|pruned
	CreatedAt   string
}

const sweepCols = `id, app_id, metric, direction, max_trials, parallel, early_stop, nodes, framework, seed, status, warning, created_by, created_at`

func scanSweep(row *sql.Row) (Sweep, error) {
	var sw Sweep
	var earlyStop int
	err := row.Scan(&sw.ID, &sw.AppID, &sw.Metric, &sw.Direction, &sw.MaxTrials, &sw.Parallel,
		&earlyStop, &sw.Nodes, &sw.Framework, &sw.Seed, &sw.Status, &sw.Warning, &sw.CreatedBy, &sw.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Sweep{}, ErrNotFound
	}
	sw.EarlyStop = earlyStop != 0
	return sw, err
}

const sweepTrialCols = `id, sweep_id, run_id, params_json, metric_value, metric_step, state, created_at`

func scanSweepTrialRows(rows *sql.Rows) (SweepTrial, error) {
	var tr SweepTrial
	err := rows.Scan(&tr.ID, &tr.SweepID, &tr.RunID, &tr.ParamsJSON, &tr.MetricValue, &tr.MetricStep, &tr.State, &tr.CreatedAt)
	return tr, err
}

// CreateSweep inserts a sweep (status "running") plus one pending trial per
// entry in trialParams, in a single transaction. trialParams entries are
// opaque JSON strings (map[string]string) produced by internal/sweep.Expand.
func (s *Store) CreateSweep(sw Sweep, trialParams []string) (Sweep, []SweepTrial, error) {
	if sw.Direction != "min" && sw.Direction != "max" {
		return Sweep{}, nil, validationErrorf("direction must be min or max, got %q", sw.Direction)
	}
	if sw.MaxTrials < 1 {
		return Sweep{}, nil, validationErrorf("max_trials must be >= 1, got %d", sw.MaxTrials)
	}
	if sw.Parallel < 1 {
		return Sweep{}, nil, validationErrorf("parallel must be >= 1, got %d", sw.Parallel)
	}
	if len(trialParams) < 1 {
		return Sweep{}, nil, validationErrorf("at least one trial is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Sweep{}, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	id := NewID()
	nodes := sw.Nodes
	if nodes < 1 {
		nodes = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO sweeps (id, app_id, metric, direction, max_trials, parallel, early_stop, nodes, framework, seed, status, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?)`,
		id, sw.AppID, sw.Metric, sw.Direction, sw.MaxTrials, sw.Parallel, sw.EarlyStop, nodes, sw.Framework, sw.Seed, sw.CreatedBy,
	); err != nil {
		return Sweep{}, nil, err
	}

	var trialIDs []string
	for _, params := range trialParams {
		tid := NewID()
		if _, err := tx.Exec(
			`INSERT INTO sweep_trials (id, sweep_id, params_json, state) VALUES (?, ?, ?, 'pending')`,
			tid, id, params,
		); err != nil {
			return Sweep{}, nil, err
		}
		trialIDs = append(trialIDs, tid)
	}

	if err := tx.Commit(); err != nil {
		return Sweep{}, nil, err
	}

	got, err := s.GetSweep(id)
	if err != nil {
		return Sweep{}, nil, err
	}
	trials, err := s.ListTrials(id)
	if err != nil {
		return Sweep{}, nil, err
	}
	return got, trials, nil
}

func (s *Store) GetSweep(id string) (Sweep, error) {
	return scanSweep(s.db.QueryRow(`SELECT `+sweepCols+` FROM sweeps WHERE id = ?`, id))
}

// ListSweeps returns an app's sweeps, newest first, capped at 50.
func (s *Store) ListSweeps(appID int64) ([]Sweep, error) {
	rows, err := s.db.Query(`SELECT `+sweepCols+` FROM sweeps WHERE app_id = ? ORDER BY rowid DESC LIMIT 50`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sweep
	for rows.Next() {
		var sw Sweep
		var earlyStop int
		if err := rows.Scan(&sw.ID, &sw.AppID, &sw.Metric, &sw.Direction, &sw.MaxTrials, &sw.Parallel,
			&earlyStop, &sw.Nodes, &sw.Framework, &sw.Seed, &sw.Status, &sw.Warning, &sw.CreatedBy, &sw.CreatedAt); err != nil {
			return nil, err
		}
		sw.EarlyStop = earlyStop != 0
		out = append(out, sw)
	}
	return out, rows.Err()
}

// ActiveSweeps returns every sweep with status "running", across all apps —
// the set the server's sweep loop drives each tick.
func (s *Store) ActiveSweeps() ([]Sweep, error) {
	rows, err := s.db.Query(`SELECT ` + sweepCols + ` FROM sweeps WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sweep
	for rows.Next() {
		var sw Sweep
		var earlyStop int
		if err := rows.Scan(&sw.ID, &sw.AppID, &sw.Metric, &sw.Direction, &sw.MaxTrials, &sw.Parallel,
			&earlyStop, &sw.Nodes, &sw.Framework, &sw.Seed, &sw.Status, &sw.Warning, &sw.CreatedBy, &sw.CreatedAt); err != nil {
			return nil, err
		}
		sw.EarlyStop = earlyStop != 0
		out = append(out, sw)
	}
	return out, rows.Err()
}

// FinishSweep marks a sweep done, stopped, or failed.
func (s *Store) FinishSweep(id, status string) error {
	if status != "done" && status != "stopped" && status != "failed" {
		return errors.New("finish status must be done, stopped, or failed")
	}
	res, err := s.db.Exec(`UPDATE sweeps SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSweepWarning sets a sticky operator-facing note on the sweep (e.g. an
// mlflow degradation).
func (s *Store) SetSweepWarning(id, warning string) error {
	res, err := s.db.Exec(`UPDATE sweeps SET warning = ? WHERE id = ?`, warning, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTrials returns a sweep's trials, oldest first (insertion order).
func (s *Store) ListTrials(sweepID string) ([]SweepTrial, error) {
	rows, err := s.db.Query(`SELECT `+sweepTrialCols+` FROM sweep_trials WHERE sweep_id = ? ORDER BY rowid ASC`, sweepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SweepTrial
	for rows.Next() {
		tr, err := scanSweepTrialRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// MarkTrialLaunched transitions a trial pending -> running once its job_runs
// row exists.
func (s *Store) MarkTrialLaunched(trialID string, runID int64) error {
	res, err := s.db.Exec(
		`UPDATE sweep_trials SET run_id = ?, state = 'running' WHERE id = ? AND state = 'pending'`,
		runID, trialID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// Distinguish "no such trial" from "trial isn't pending".
		if _, getErr := s.getTrial(trialID); errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		return errors.New("trial not pending")
	}
	return nil
}

// FinishTrial transitions a running trial to done, failed, or pruned. value
// and step are nullable (e.g. a pruned trial killed before it reported).
// A pending trial may also go straight to failed: a launch that errors
// permanently (app no longer deployed, render failure) never reaches
// running, and leaving it pending would retry it every tick forever.
func (s *Store) FinishTrial(trialID, state string, value *float64, step *int64) error {
	if state != "done" && state != "failed" && state != "pruned" {
		return errors.New("finish state must be done, failed, or pruned")
	}
	var v, st any
	if value != nil {
		v = *value
	}
	if step != nil {
		st = *step
	}
	res, err := s.db.Exec(
		`UPDATE sweep_trials SET state = ?, metric_value = ?, metric_step = ? WHERE id = ?
		 AND (state = 'running' OR (state = 'pending' AND ? = 'failed'))`,
		state, v, st, trialID, state)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		if _, getErr := s.getTrial(trialID); errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		return errors.New("trial not running")
	}
	return nil
}

func (s *Store) getTrial(trialID string) (SweepTrial, error) {
	rows, err := s.db.Query(`SELECT `+sweepTrialCols+` FROM sweep_trials WHERE id = ?`, trialID)
	if err != nil {
		return SweepTrial{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return SweepTrial{}, ErrNotFound
	}
	return scanSweepTrialRows(rows)
}
