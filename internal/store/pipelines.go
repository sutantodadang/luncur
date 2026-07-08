package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Pipeline is a stored pipeline.yaml plus its trigger/engine configuration.
// Cron and WebhookSecret are written by C2 (cron/webhook triggers); both are
// zero-valued for every pipeline created under C1 (manual trigger only).
type Pipeline struct {
	ID            string
	ProjectID     int64
	Name          string
	YAML          string
	Cron          string // written by C2; '' in C1
	WebhookSecret []byte // sealed; nil until C2
	Engine        string // ''=follow global setting
	CreatedBy     sql.NullInt64
	CreatedAt     string
}

// PipelineRun is one triggered execution of a Pipeline. SpecJSON is the
// compiled pipeline.Spec snapshot at run start — immutable, so editing the
// pipeline never mutates an in-flight run.
type PipelineRun struct {
	ID         string
	PipelineID string
	Status     string // running|done|failed|stopped
	SpecJSON   string
	Trigger    string // manual|cron|webhook (C1 writes only manual)
	Warning    string
	StartedAt  string
	FinishedAt sql.NullString
}

// PipelineRunStep is one pre-expanded step row within a PipelineRun,
// created at run start from the compiled spec (sweep_trials pattern).
type PipelineRunStep struct {
	ID         string
	RunID      string
	Name       string
	Kind       string // app|image|deploy|scale|notify
	State      string // pending|running|done|failed|skipped
	JobRunID   sql.NullInt64
	Attempt    int
	Detail     string
	StartedAt  sql.NullString
	FinishedAt sql.NullString
}

const pipelineCols = `id, project_id, name, yaml, cron, webhook_secret, engine, created_by, created_at`

func scanPipeline(row *sql.Row) (Pipeline, error) {
	var pl Pipeline
	err := row.Scan(&pl.ID, &pl.ProjectID, &pl.Name, &pl.YAML, &pl.Cron, &pl.WebhookSecret, &pl.Engine, &pl.CreatedBy, &pl.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Pipeline{}, ErrNotFound
	}
	return pl, err
}

const pipelineRunCols = `id, pipeline_id, status, spec_json, trigger, warning, started_at, finished_at`

func scanPipelineRun(row *sql.Row) (PipelineRun, error) {
	var r PipelineRun
	err := row.Scan(&r.ID, &r.PipelineID, &r.Status, &r.SpecJSON, &r.Trigger, &r.Warning, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineRun{}, ErrNotFound
	}
	return r, err
}

const pipelineRunStepCols = `id, run_id, name, kind, state, job_run_id, attempt, detail, started_at, finished_at`

func scanRunStepRows(rows *sql.Rows) (PipelineRunStep, error) {
	var st PipelineRunStep
	err := rows.Scan(&st.ID, &st.RunID, &st.Name, &st.Kind, &st.State, &st.JobRunID, &st.Attempt, &st.Detail, &st.StartedAt, &st.FinishedAt)
	return st, err
}

// CreatePipeline inserts a pipeline. ID is generated (NewID); Name must
// satisfy the same DNS-1123 regex projects use (it embeds in Job names via
// PipelineStepJob), and must be unique within the project.
func (s *Store) CreatePipeline(pl Pipeline) (Pipeline, error) {
	if !validName(pl.Name) {
		return Pipeline{}, validationErrorf("invalid pipeline name %q (lowercase letters, digits, dashes; max 40 chars)", pl.Name)
	}
	id := NewID()
	_, err := s.db.Exec(
		`INSERT INTO pipelines (id, project_id, name, yaml, cron, engine, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, pl.ProjectID, pl.Name, pl.YAML, pl.Cron, pl.Engine, pl.CreatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Pipeline{}, fmt.Errorf("pipeline %q already exists in this project", pl.Name)
		}
		return Pipeline{}, err
	}
	return s.getPipelineByID(id)
}

func (s *Store) getPipelineByID(id string) (Pipeline, error) {
	return scanPipeline(s.db.QueryRow(`SELECT `+pipelineCols+` FROM pipelines WHERE id = ?`, id))
}

// GetPipeline looks up a pipeline by its project-scoped name.
func (s *Store) GetPipeline(projectID int64, name string) (Pipeline, error) {
	return scanPipeline(s.db.QueryRow(`SELECT `+pipelineCols+` FROM pipelines WHERE project_id = ? AND name = ?`, projectID, name))
}

// GetPipelineByID looks up a pipeline by its primary key, for code that only
// has a run's PipelineID foreign key and not the owning project's name (the
// engine loop; mirrors GetAppByID's reason for existing).
func (s *Store) GetPipelineByID(id string) (Pipeline, error) {
	return s.getPipelineByID(id)
}

// ListPipelines returns a project's pipelines, name ascending.
func (s *Store) ListPipelines(projectID int64) ([]Pipeline, error) {
	rows, err := s.db.Query(`SELECT `+pipelineCols+` FROM pipelines WHERE project_id = ? ORDER BY name ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pipeline
	for rows.Next() {
		var pl Pipeline
		if err := rows.Scan(&pl.ID, &pl.ProjectID, &pl.Name, &pl.YAML, &pl.Cron, &pl.WebhookSecret, &pl.Engine, &pl.CreatedBy, &pl.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

// CronPipelines returns every pipeline with a non-empty cron, across all
// projects — the set the server's pipeline loop checks for due-ness each
// tick (firePipelineCrons).
func (s *Store) CronPipelines() ([]Pipeline, error) {
	rows, err := s.db.Query(`SELECT ` + pipelineCols + ` FROM pipelines WHERE cron != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pipeline
	for rows.Next() {
		var pl Pipeline
		if err := rows.Scan(&pl.ID, &pl.ProjectID, &pl.Name, &pl.YAML, &pl.Cron, &pl.WebhookSecret, &pl.Engine, &pl.CreatedBy, &pl.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, rows.Err()
}

// UpdatePipeline replaces a pipeline's yaml/cron/engine. Existing in-flight
// runs are unaffected — their spec_json snapshot is immutable.
func (s *Store) UpdatePipeline(id, yaml, cron, engine string) error {
	res, err := s.db.Exec(`UPDATE pipelines SET yaml = ?, cron = ?, engine = ? WHERE id = ?`, yaml, cron, engine, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePipeline removes the pipeline row; its runs and run steps cascade
// via FK (pipeline_runs -> pipelines, pipeline_run_steps -> pipeline_runs).
func (s *Store) DeletePipeline(id string) error {
	res, err := s.db.Exec(`DELETE FROM pipelines WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// CreatePipelineRun inserts a run (status "running") plus one pending step
// row per [name,kind] pair in stepNamesKinds, in a single transaction.
// Insertion order is preserved (rowid), giving ListRunSteps a stable
// topo-order source.
func (s *Store) CreatePipelineRun(run PipelineRun, stepNamesKinds [][2]string) (PipelineRun, []PipelineRunStep, error) {
	if run.Trigger != "manual" && run.Trigger != "cron" && run.Trigger != "webhook" {
		return PipelineRun{}, nil, validationErrorf("trigger must be manual, cron, or webhook, got %q", run.Trigger)
	}
	if len(stepNamesKinds) < 1 {
		return PipelineRun{}, nil, validationErrorf("at least one step is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return PipelineRun{}, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	id := NewID()
	if _, err := tx.Exec(
		`INSERT INTO pipeline_runs (id, pipeline_id, status, spec_json, trigger) VALUES (?, ?, 'running', ?, ?)`,
		id, run.PipelineID, run.SpecJSON, run.Trigger,
	); err != nil {
		return PipelineRun{}, nil, err
	}

	for _, nk := range stepNamesKinds {
		sid := NewID()
		if _, err := tx.Exec(
			`INSERT INTO pipeline_run_steps (id, run_id, name, kind, state) VALUES (?, ?, ?, ?, 'pending')`,
			sid, id, nk[0], nk[1],
		); err != nil {
			return PipelineRun{}, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return PipelineRun{}, nil, err
	}

	got, err := s.GetPipelineRun(id)
	if err != nil {
		return PipelineRun{}, nil, err
	}
	steps, err := s.ListRunSteps(id)
	if err != nil {
		return PipelineRun{}, nil, err
	}
	return got, steps, nil
}

func (s *Store) GetPipelineRun(id string) (PipelineRun, error) {
	return scanPipelineRun(s.db.QueryRow(`SELECT `+pipelineRunCols+` FROM pipeline_runs WHERE id = ?`, id))
}

// ListPipelineRuns returns a pipeline's runs, newest first, capped at 50.
func (s *Store) ListPipelineRuns(pipelineID string) ([]PipelineRun, error) {
	rows, err := s.db.Query(`SELECT `+pipelineRunCols+` FROM pipeline_runs WHERE pipeline_id = ? ORDER BY rowid DESC LIMIT 50`, pipelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineRun
	for rows.Next() {
		var r PipelineRun
		if err := rows.Scan(&r.ID, &r.PipelineID, &r.Status, &r.SpecJSON, &r.Trigger, &r.Warning, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActivePipelineRuns returns every run with status "running", across all
// pipelines — the set the server's pipeline loop drives each tick.
func (s *Store) ActivePipelineRuns() ([]PipelineRun, error) {
	rows, err := s.db.Query(`SELECT ` + pipelineRunCols + ` FROM pipeline_runs WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineRun
	for rows.Next() {
		var r PipelineRun
		if err := rows.Scan(&r.ID, &r.PipelineID, &r.Status, &r.SpecJSON, &r.Trigger, &r.Warning, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FinishPipelineRun marks a run done, stopped, or failed.
func (s *Store) FinishPipelineRun(id, status string) error {
	if status != "done" && status != "stopped" && status != "failed" {
		return errors.New("finish status must be done, stopped, or failed")
	}
	res, err := s.db.Exec(`UPDATE pipeline_runs SET status = ?, finished_at = datetime('now') WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPipelineRunWarning sets a sticky operator-facing note on the run (e.g.
// an mlflow degradation, sweep-warning pattern).
func (s *Store) SetPipelineRunWarning(id, warning string) error {
	res, err := s.db.Exec(`UPDATE pipeline_runs SET warning = ? WHERE id = ?`, warning, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRunSteps returns a run's steps in insertion (rowid) order — the
// topo-order source recorded at CreatePipelineRun time.
func (s *Store) ListRunSteps(runID string) ([]PipelineRunStep, error) {
	rows, err := s.db.Query(`SELECT `+pipelineRunStepCols+` FROM pipeline_run_steps WHERE run_id = ? ORDER BY rowid ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelineRunStep
	for rows.Next() {
		st, err := scanRunStepRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// MarkStepRunning transitions a step pending -> running (first launch) or
// running -> running (retry: attempt bump, new jobRunID). jobRunID is nil
// for kind=image steps (no job_runs row) and for the initial launch of a
// kind=app step before its job_runs row is known by the caller.
func (s *Store) MarkStepRunning(stepID string, jobRunID *int64, attempt int) error {
	var jr any
	if jobRunID != nil {
		jr = *jobRunID
	}
	res, err := s.db.Exec(
		`UPDATE pipeline_run_steps
		 SET state = 'running', job_run_id = ?, attempt = ?, started_at = COALESCE(started_at, datetime('now'))
		 WHERE id = ? AND state IN ('pending','running')`,
		jr, attempt, stepID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		if _, getErr := s.getRunStep(stepID); errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		return errors.New("step not pending or running")
	}
	return nil
}

// FinishStep transitions a step to done, failed, or skipped.
//   - done is only legal from running (a step must have launched to
//     succeed).
//   - failed and skipped are legal from running OR pending — a step that
//     never launches (unlaunchable app/render failure, or a fail-fast
//     downstream skip) must be able to leave pending directly, or it would
//     be retried/reconsidered every tick forever (B2 FinishTrial lesson).
func (s *Store) FinishStep(stepID, state, detail string) error {
	var guard string
	switch state {
	case "done":
		guard = `state = 'running'`
	case "failed", "skipped":
		guard = `state IN ('running','pending')`
	default:
		return errors.New("finish state must be done, failed, or skipped")
	}
	res, err := s.db.Exec(
		`UPDATE pipeline_run_steps SET state = ?, detail = ?, finished_at = datetime('now') WHERE id = ? AND (`+guard+`)`,
		state, detail, stepID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		if _, getErr := s.getRunStep(stepID); errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		return errors.New("step not in a finishable state")
	}
	return nil
}

func (s *Store) getRunStep(stepID string) (PipelineRunStep, error) {
	rows, err := s.db.Query(`SELECT `+pipelineRunStepCols+` FROM pipeline_run_steps WHERE id = ?`, stepID)
	if err != nil {
		return PipelineRunStep{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return PipelineRunStep{}, ErrNotFound
	}
	return scanRunStepRows(rows)
}
