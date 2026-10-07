package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AppPolicy is an app's reliability and rollout policy (app_policies). A
// missing row reads as DefaultAppPolicy, so every existing app gets the
// defaults without a backfill.
type AppPolicy struct {
	AppID int64 `json:"-"`
	// AutoRollback rolls a deploy whose rollout fails back to the previous
	// live deploy.
	AutoRollback bool `json:"auto_rollback"`
	// RolloutTimeout is how long (seconds) the rollout gate waits for the new
	// pods; it is also rendered as progressDeadlineSeconds.
	RolloutTimeout int `json:"rollout_timeout"`
	// Probe is "auto" (TCP readiness when no health path is set) or "off".
	Probe string `json:"probe"`
	// Security is the pod security level: baseline, restricted or relaxed.
	Security string `json:"security"`
	// Strategy is rolling, canary or bluegreen.
	Strategy string `json:"strategy"`
	// CanarySteps are the traffic weights (percent) a canary steps through
	// before promotion, e.g. [10 50].
	CanarySteps []int `json:"canary_steps"`
	// CanaryInterval is how long (seconds) each canary step holds.
	CanaryInterval int `json:"canary_interval"`
	// CanaryMinSuccess is the minimum probe success rate (percent) a canary
	// step must keep.
	CanaryMinSuccess int `json:"canary_min_success"`
	// BlueGreenKeep is how long (seconds) the old track stays scaled after a
	// blue-green switch, for an instant rollback.
	BlueGreenKeep int `json:"bluegreen_keep"`
}

// Policy enums, shared with the server's validation and render.
var (
	PolicyProbes     = []string{"auto", "off"}
	PolicySecurity   = []string{"baseline", "restricted", "relaxed"}
	PolicyStrategies = []string{"rolling", "canary", "bluegreen"}
)

// DefaultAppPolicy is the policy of an app with no app_policies row.
func DefaultAppPolicy(appID int64) AppPolicy {
	return AppPolicy{
		AppID: appID, AutoRollback: true, RolloutTimeout: 300, Probe: "auto",
		Security: "baseline", Strategy: "rolling", CanarySteps: []int{10, 50},
		CanaryInterval: 120, CanaryMinSuccess: 99, BlueGreenKeep: 600,
	}
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// ParseCanarySteps parses "10,50" into ascending weights in 1..99.
func ParseCanarySteps(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("canary steps must list at least one weight, e.g. 10,50")
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), "%")))
		if err != nil || n < 1 || n > 99 {
			return nil, fmt.Errorf("canary step %q must be a percentage between 1 and 99", part)
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			return nil, fmt.Errorf("canary steps must increase (got %s)", s)
		}
		out = append(out, n)
	}
	if len(out) > 5 {
		return nil, fmt.Errorf("at most 5 canary steps")
	}
	return out, nil
}

// FormatCanarySteps is ParseCanarySteps' inverse.
func FormatCanarySteps(steps []int) string {
	parts := make([]string, len(steps))
	for i, n := range steps {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// Validate checks every field's range.
func (p AppPolicy) Validate() error {
	switch {
	case p.RolloutTimeout < 30 || p.RolloutTimeout > 3600:
		return fmt.Errorf("rollout timeout must be between 30s and 1h")
	case !oneOf(p.Probe, PolicyProbes):
		return fmt.Errorf("probe must be one of %s", strings.Join(PolicyProbes, ", "))
	case !oneOf(p.Security, PolicySecurity):
		return fmt.Errorf("security must be one of %s", strings.Join(PolicySecurity, ", "))
	case !oneOf(p.Strategy, PolicyStrategies):
		return fmt.Errorf("strategy must be one of %s", strings.Join(PolicyStrategies, ", "))
	case p.CanaryInterval < 10 || p.CanaryInterval > 3600:
		return fmt.Errorf("canary interval must be between 10s and 1h")
	case p.CanaryMinSuccess < 50 || p.CanaryMinSuccess > 100:
		return fmt.Errorf("canary minimum success must be between 50 and 100 percent")
	case p.BlueGreenKeep < 0 || p.BlueGreenKeep > 86400:
		return fmt.Errorf("blue-green keep must be between 0 and 24h")
	}
	if _, err := ParseCanarySteps(FormatCanarySteps(p.CanarySteps)); err != nil {
		return err
	}
	return nil
}

// GetAppPolicy returns the app's policy, or DefaultAppPolicy when it has
// none stored.
func (s *Store) GetAppPolicy(appID int64) (AppPolicy, error) {
	p := AppPolicy{AppID: appID}
	var steps string
	err := s.db.QueryRow(
		`SELECT auto_rollback, rollout_timeout, probe, security, strategy, canary_steps, canary_interval, canary_min_success, bluegreen_keep
		 FROM app_policies WHERE app_id = ?`, appID,
	).Scan(&p.AutoRollback, &p.RolloutTimeout, &p.Probe, &p.Security, &p.Strategy, &steps, &p.CanaryInterval, &p.CanaryMinSuccess, &p.BlueGreenKeep)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultAppPolicy(appID), nil
	}
	if err != nil {
		return AppPolicy{}, err
	}
	if p.CanarySteps, err = ParseCanarySteps(steps); err != nil {
		p.CanarySteps = DefaultAppPolicy(appID).CanarySteps
	}
	return p, nil
}

// SetAppPolicy validates and upserts the app's policy.
func (s *Store) SetAppPolicy(p AppPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO app_policies (app_id, auto_rollback, rollout_timeout, probe, security, strategy, canary_steps, canary_interval, canary_min_success, bluegreen_keep)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id) DO UPDATE SET
		   auto_rollback = excluded.auto_rollback, rollout_timeout = excluded.rollout_timeout,
		   probe = excluded.probe, security = excluded.security, strategy = excluded.strategy,
		   canary_steps = excluded.canary_steps, canary_interval = excluded.canary_interval,
		   canary_min_success = excluded.canary_min_success, bluegreen_keep = excluded.bluegreen_keep`,
		p.AppID, p.AutoRollback, p.RolloutTimeout, p.Probe, p.Security, p.Strategy,
		FormatCanarySteps(p.CanarySteps), p.CanaryInterval, p.CanaryMinSuccess, p.BlueGreenKeep)
	return err
}

// CopyAppPolicy copies one app's stored policy to another (preview clones,
// environment copies). No stored policy copies nothing.
func (s *Store) CopyAppPolicy(fromAppID, toAppID int64) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO app_policies (app_id, auto_rollback, rollout_timeout, probe, security, strategy, canary_steps, canary_interval, canary_min_success, bluegreen_keep)
		 SELECT ?, auto_rollback, rollout_timeout, probe, security, strategy, canary_steps, canary_interval, canary_min_success, bluegreen_keep
		 FROM app_policies WHERE app_id = ?`, toAppID, fromAppID)
	return err
}

// DeployOutcome is what the rollout gate recorded for a deploy.
type DeployOutcome struct {
	FailReason string `json:"fail_reason,omitempty"`
	ReadyAt    string `json:"ready_at,omitempty"`
	// Ready/Want are the last observed new-pod counts while deploying.
	Ready int `json:"ready"`
	Want  int `json:"want"`
}

// GetDeployOutcome returns the deploy's outcome (zero value when none).
func (s *Store) GetDeployOutcome(deployID string) (DeployOutcome, error) {
	var o DeployOutcome
	err := s.db.QueryRow(`SELECT fail_reason, ready_at, ready, want FROM deployment_outcomes WHERE deploy_id = ?`, deployID).
		Scan(&o.FailReason, &o.ReadyAt, &o.Ready, &o.Want)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployOutcome{}, nil
	}
	return o, err
}

// SetDeployProgress records the gate's latest ready/want pod counts.
func (s *Store) SetDeployProgress(deployID string, ready, want int) error {
	_, err := s.db.Exec(
		`INSERT INTO deployment_outcomes (deploy_id, ready, want) VALUES (?, ?, ?)
		 ON CONFLICT (deploy_id) DO UPDATE SET ready = excluded.ready, want = excluded.want`,
		deployID, ready, want)
	return err
}

// SetDeployFailReason records why a deploy failed.
func (s *Store) SetDeployFailReason(deployID, reason string) error {
	_, err := s.db.Exec(
		`INSERT INTO deployment_outcomes (deploy_id, fail_reason) VALUES (?, ?)
		 ON CONFLICT (deploy_id) DO UPDATE SET fail_reason = excluded.fail_reason`,
		deployID, reason)
	return err
}

// SetDeployReady records when a deploy's new pods all became available.
func (s *Store) SetDeployReady(deployID string, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO deployment_outcomes (deploy_id, ready_at) VALUES (?, ?)
		 ON CONFLICT (deploy_id) DO UPDATE SET ready_at = excluded.ready_at`,
		deployID, at.UTC().Format(time.RFC3339))
	return err
}
