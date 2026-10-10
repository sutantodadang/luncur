package store

import (
	"database/sql"
	"errors"
)

// Rollout is a canary / blue-green deploy's step-machine state (rollouts).
//
// Phases: starting (canary track rolling out) → stepping (traffic weights)
// → promoting (stable rolled to the new image) → done; or aborted.
// promote-requested / abort-requested are set by the API and acted on by
// the step loop at its next tick.
type Rollout struct {
	DeployID      string `json:"deploy_id"`
	AppID         int64  `json:"-"`
	Strategy      string `json:"strategy"`
	Step          int    `json:"step"`
	Weight        int    `json:"weight"`
	Phase         string `json:"phase"`
	StepStartedAt string `json:"step_started_at"`
	ProbesOK      int    `json:"probes_ok"`
	ProbesTotal   int    `json:"probes_total"`
	Note          string `json:"note,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

// RolloutActive reports whether a phase still needs the step loop.
func RolloutActive(phase string) bool {
	return phase != "done" && phase != "aborted"
}

const rolloutCols = `deploy_id, app_id, strategy, step, weight, phase, step_started_at, probes_ok, probes_total, note, updated_at`

func scanRollout(sc rowScanner) (Rollout, error) {
	var r Rollout
	err := sc.Scan(&r.DeployID, &r.AppID, &r.Strategy, &r.Step, &r.Weight, &r.Phase, &r.StepStartedAt, &r.ProbesOK, &r.ProbesTotal, &r.Note, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Rollout{}, ErrNotFound
	}
	return r, err
}

// CreateRollout starts a deploy's rollout record in phase "starting".
func (s *Store) CreateRollout(deployID string, appID int64, strategy string) (Rollout, error) {
	if _, err := s.db.Exec(`INSERT INTO rollouts (deploy_id, app_id, strategy) VALUES (?, ?, ?)`, deployID, appID, strategy); err != nil {
		return Rollout{}, err
	}
	return s.GetRollout(deployID)
}

// GetRollout returns a deploy's rollout record.
func (s *Store) GetRollout(deployID string) (Rollout, error) {
	return scanRollout(s.db.QueryRow(`SELECT `+rolloutCols+` FROM rollouts WHERE deploy_id = ?`, deployID))
}

// ActiveRollout returns the app's newest rollout still in progress.
func (s *Store) ActiveRollout(appID int64) (Rollout, error) {
	return scanRollout(s.db.QueryRow(`SELECT `+rolloutCols+` FROM rollouts
		WHERE app_id = ? AND phase NOT IN ('done','aborted') ORDER BY rowid DESC LIMIT 1`, appID))
}

// SetRolloutStep moves the rollout to step/weight (phase stepping) and
// resets the step's probe tally and clock.
func (s *Store) SetRolloutStep(deployID string, step, weight int) error {
	_, err := s.db.Exec(`UPDATE rollouts SET step = ?, weight = ?, phase = 'stepping', probes_ok = 0, probes_total = 0,
		step_started_at = datetime('now'), updated_at = datetime('now') WHERE deploy_id = ?`, step, weight, deployID)
	return err
}

// SetRolloutPhase sets the phase (and note, when non-empty).
func (s *Store) SetRolloutPhase(deployID, phase, note string) error {
	_, err := s.db.Exec(`UPDATE rollouts SET phase = ?, note = CASE WHEN ? = '' THEN note ELSE ? END,
		updated_at = datetime('now') WHERE deploy_id = ?`, phase, note, note, deployID)
	return err
}

// RequestRolloutPhase records a promote/abort request, only while the
// rollout is still running (returns false otherwise).
func (s *Store) RequestRolloutPhase(deployID, phase string) (bool, error) {
	res, err := s.db.Exec(`UPDATE rollouts SET phase = ?, updated_at = datetime('now')
		WHERE deploy_id = ? AND phase IN ('starting','stepping')`, phase, deployID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AddRolloutProbe tallies one canary probe for the current step.
func (s *Store) AddRolloutProbe(deployID string, ok bool) error {
	okN := 0
	if ok {
		okN = 1
	}
	_, err := s.db.Exec(`UPDATE rollouts SET probes_ok = probes_ok + ?, probes_total = probes_total + 1,
		updated_at = datetime('now') WHERE deploy_id = ?`, okN, deployID)
	return err
}
