package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/addon"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
	"github.com/sutantodadang/luncur/internal/sweep"
)

// sweepLoopInterval is startSweepLoop's tick cadence — the same lifecycle
// shape as the metrics monitor sampler (see monitor.go).
const sweepLoopInterval = 30 * time.Second

// trialView is decideSweep's pure-core input per trial: current store state
// plus the live run/metric observation gathered this tick (Run is nil while
// the trial is still pending; Live is the zero Obs unless the sweep has
// early_stop and the trial is running).
type trialView struct {
	Trial store.SweepTrial
	Run   *store.JobRun
	Live  sweep.Obs
}

// sweepActions is one tick's pure decision: which pending trials to launch,
// which running trials to prune, and whether the sweep has fully drained.
type sweepActions struct {
	Launch []string
	Prune  []string
	Finish bool
}

// decideSweep is the pure per-tick decision core — no store/kube calls.
// Rules (source of truth: docs/superpowers/plans/2026-07-07-b2-sweeps.md
// Task 4):
//  1. running := trials in state "running"; pending := "pending".
//  2. free := sw.Parallel - len(running); Launch = first `free` pending
//     trial ids, oldest first (trials is assumed oldest-first, matching
//     store.ListTrials).
//  3. Prune (only when sw.EarlyStop): done := trials "done" with a non-null
//     metric. If len(done) >= 3: median := median of done values; a running
//     trial with Live.Found and (direction min: Live.Value > median; max:
//     Live.Value < median) at Live.Step >= minDoneStep (smallest final step
//     among done trials, 0 when none recorded) is pruned.
//  4. Finish: true when pending and running are both empty on input.
//  5. failed/pruned/done trials are inert.
func decideSweep(sw store.Sweep, trials []trialView) sweepActions {
	var running, pending []trialView
	var done []trialView
	for _, tv := range trials {
		switch tv.Trial.State {
		case "running":
			running = append(running, tv)
		case "pending":
			pending = append(pending, tv)
		case "done":
			if tv.Trial.MetricValue.Valid {
				done = append(done, tv)
			}
		}
	}

	actions := sweepActions{Finish: len(pending) == 0 && len(running) == 0}

	if free := sw.Parallel - len(running); free > 0 {
		for i := 0; i < free && i < len(pending); i++ {
			actions.Launch = append(actions.Launch, pending[i].Trial.ID)
		}
	}

	if !sw.EarlyStop || len(done) < 3 {
		return actions
	}

	doneValues := make([]float64, len(done))
	var minDoneStep int64
	first := true
	for i, tv := range done {
		doneValues[i] = tv.Trial.MetricValue.Float64
		if tv.Trial.MetricStep.Valid {
			if first || tv.Trial.MetricStep.Int64 < minDoneStep {
				minDoneStep = tv.Trial.MetricStep.Int64
				first = false
			}
		}
	}
	median := medianOf(doneValues)

	for _, tv := range running {
		if !tv.Live.Found || tv.Live.Step < minDoneStep {
			continue
		}
		worse := false
		switch sw.Direction {
		case "min":
			worse = tv.Live.Value > median
		case "max":
			worse = tv.Live.Value < median
		}
		if worse {
			actions.Prune = append(actions.Prune, tv.Trial.ID)
		}
	}
	return actions
}

// medianOf returns the median of a non-empty slice; vals is copied before
// sorting so the caller's slice order is untouched.
func medianOf(vals []float64) float64 {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// trialEnv is the env a trial's run carries: every param plus ids and, when
// the project has an mlflow addon, MLFLOW_TRACKING_URI/RUN_NAME.
func trialEnv(sw store.Sweep, tr store.SweepTrial, mlflowURL string) map[string]string {
	env := map[string]string{
		"LUNCUR_SWEEP_ID": sw.ID,
		"LUNCUR_TRIAL_ID": tr.ID,
	}
	var params map[string]string
	_ = json.Unmarshal([]byte(tr.ParamsJSON), &params)
	for k, v := range params {
		env["LUNCUR_PARAM_"+strings.ToUpper(k)] = v
	}
	if mlflowURL != "" {
		env["MLFLOW_TRACKING_URI"] = mlflowURL
		env["MLFLOW_RUN_NAME"] = "trial-" + tr.ID // VERIFY(mlflow-field): client honors MLFLOW_RUN_NAME
	}
	return env
}

// sweepAppProject loads a sweep's app + project. Logs and returns ok=false
// on failure (e.g. the app was deleted out from under a running sweep).
func (s *server) sweepAppProject(sw store.Sweep) (store.App, store.Project, bool) {
	a, err := s.st.GetAppByID(sw.AppID)
	if err != nil {
		log.Printf("sweep %s: get app %d: %v", sw.ID, sw.AppID, err)
		return store.App{}, store.Project{}, false
	}
	p, err := s.st.GetProjectByID(a.ProjectID)
	if err != nil {
		log.Printf("sweep %s: get project %d: %v", sw.ID, a.ProjectID, err)
		return store.App{}, store.Project{}, false
	}
	return a, p, true
}

// sweepMLflowURL returns the in-cluster URL of the app's first attached
// mlflow addon, or "" when none is attached — mirrors addonEnv's own
// per-app addon lookup (addons.go).
func (s *server) sweepMLflowURL(a store.App, namespace string) string {
	addons, err := s.st.AddonsForApp(a.ID)
	if err != nil {
		return ""
	}
	for _, ad := range addons {
		if ad.Type == "mlflow" {
			_, url := addonKeyURL(ad.Type, ad.Name, namespace, addon.Creds{})
			return url
		}
	}
	return ""
}

// trialMetric reads a trial's current metric observation: MLflow when the
// app has an attached mlflow addon (until it degrades for the rest of this
// sweep), else the log-line contract read from the trial's pod(s). Used
// both mid-run (early stopping) and at harvest (trial finished).
func (s *server) trialMetric(ctx context.Context, sw store.Sweep, tr store.SweepTrial, app store.App, project store.Project, run store.JobRun, mlflowURL string) sweep.Obs {
	if mlflowURL != "" && !s.sweepMLflowDown[sw.ID] {
		mf := &sweep.MLflow{BaseURL: mlflowURL}
		obs, err := mf.Latest(ctx, "trial-"+tr.ID, sw.Metric)
		if err == nil {
			return obs
		}
		log.Printf("sweep %s: mlflow unreachable, degrading to log-line metrics: %v", sw.ID, err)
		if s.sweepMLflowDown == nil {
			s.sweepMLflowDown = map[string]bool{}
		}
		s.sweepMLflowDown[sw.ID] = true
		if sw.Warning == "" {
			warn := "mlflow unreachable — using log-line metrics: " + err.Error()
			if e := s.st.SetSweepWarning(sw.ID, warn); e != nil {
				log.Printf("sweep %s: set warning: %v", sw.ID, e)
			}
		}
	}

	if s.kube == nil {
		return sweep.Obs{}
	}
	jobName := jobRunName(app.Name, run.ID)
	pods, err := s.kube.JobPods(ctx, project.Namespace, jobName)
	if err != nil || len(pods) == 0 {
		return sweep.Obs{}
	}
	var best sweep.Obs
	for _, pod := range pods {
		rc, err := s.kube.PodLogStream(ctx, project.Namespace, pod, false, 0, 0)
		if err != nil {
			continue
		}
		obs := sweep.ParseMetricLines(rc, sw.Metric)
		rc.Close()
		if obs.Found && (!best.Found || obs.Step >= best.Step) {
			best = obs
		}
	}
	return best
}

// sweepTick drives one tick of every active sweep: harvest finished trials'
// metrics, decide launches/prunes/finish via decideSweep, and apply them.
func (s *server) sweepTick(ctx context.Context) {
	sweeps, err := s.st.ActiveSweeps()
	if err != nil {
		log.Printf("sweep tick: list active sweeps: %v", err)
		return
	}
	for _, sw := range sweeps {
		s.sweepTickOne(ctx, sw)
	}
}

func (s *server) sweepTickOne(ctx context.Context, sw store.Sweep) {
	trials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		log.Printf("sweep %s: list trials: %v", sw.ID, err)
		return
	}
	app, project, ok := s.sweepAppProject(sw)
	if !ok {
		return
	}
	mlflowURL := s.sweepMLflowURLFn(app, project.Namespace)

	views := make([]trialView, len(trials))
	viewByID := make(map[string]trialView, len(trials))
	for i, tr := range trials {
		tv := trialView{Trial: tr}
		if tr.RunID.Valid {
			run, err := s.st.GetJobRun(tr.RunID.Int64)
			if err != nil {
				log.Printf("sweep %s: get run for trial %s: %v", sw.ID, tr.ID, err)
			} else {
				runCopy := run
				tv.Run = &runCopy
				if tr.State == "running" {
					if run.Status == "running" {
						if sw.EarlyStop {
							tv.Live = s.trialMetric(ctx, sw, tr, app, project, run, mlflowURL)
						}
					} else {
						// Run finished: harvest its final metric and close
						// out the trial (done/failed mirrors the run's own
						// terminal status).
						obs := s.trialMetric(ctx, sw, tr, app, project, run, mlflowURL)
						state := "done"
						if run.Status == "failed" {
							state = "failed"
						}
						var val *float64
						var step *int64
						if obs.Found {
							v, st2 := obs.Value, obs.Step
							val, step = &v, &st2
						}
						if err := s.st.FinishTrial(tr.ID, state, val, step); err != nil {
							log.Printf("sweep %s: finish trial %s: %v", sw.ID, tr.ID, err)
						} else {
							tv.Trial.State = state
							if obs.Found {
								tv.Trial.MetricValue = sql.NullFloat64{Float64: obs.Value, Valid: true}
								tv.Trial.MetricStep = sql.NullInt64{Int64: obs.Step, Valid: true}
							}
							tv.Live = obs
						}
					}
				}
			}
		}
		views[i] = tv
		viewByID[tr.ID] = tv
	}

	actions := decideSweep(sw, views)

	for _, id := range actions.Launch {
		if tv, ok := viewByID[id]; ok {
			s.sweepLaunchTrial(ctx, sw, tv.Trial, app, project, mlflowURL)
		}
	}
	for _, id := range actions.Prune {
		if tv, ok := viewByID[id]; ok {
			s.sweepPruneTrial(ctx, sw, tv, app, project)
		}
	}
	if actions.Finish {
		if err := s.st.FinishSweep(sw.ID, "done"); err != nil {
			log.Printf("sweep %s: finish sweep: %v", sw.ID, err)
		}
	}
}

// sweepLaunchTrial starts one trial's run via B1's startRun, sharing its
// GPU-budget pacing: errRunOverBudget leaves the trial pending for a later
// tick (once other trials free up budget), exactly like a request that hit
// quota gets retried by the caller.
func (s *server) sweepLaunchTrial(ctx context.Context, sw store.Sweep, tr store.SweepTrial, app store.App, project store.Project, mlflowURL string) {
	run, err := s.startRun(ctx, project, app, runOpts{
		Nodes:     sw.Nodes,
		Framework: sw.Framework,
		Env:       trialEnv(sw, tr, mlflowURL),
	})
	switch {
	case errors.Is(err, errRunOverBudget):
		log.Printf("sweep %s: trial %s over gpu budget this tick, left pending: %v", sw.ID, tr.ID, err)
		return
	case err != nil:
		log.Printf("sweep %s: launch trial %s: %v", sw.ID, tr.ID, err)
		// A non-budget launch error is permanent (errNotDeployed, render or
		// apply failure) — mark the trial failed so it isn't retried every
		// tick forever. FinishTrial allows pending -> failed for exactly
		// this path.
		if e := s.st.FinishTrial(tr.ID, "failed", nil, nil); e != nil {
			log.Printf("sweep %s: trial %s -> failed: %v", sw.ID, tr.ID, e)
		}
		return
	}
	if err := s.st.MarkTrialLaunched(tr.ID, run.ID); err != nil {
		log.Printf("sweep %s: mark trial %s launched: %v", sw.ID, tr.ID, err)
	}
}

// sweepPruneTrial kills a running trial's job early (median early-stopping).
// The run row records pod-level truth — it was killed on purpose, not
// "succeeded", so it's marked "failed" — while the trial's "pruned" state is
// the sweep-level truth surfaced to operators.
func (s *server) sweepPruneTrial(ctx context.Context, sw store.Sweep, tv trialView, app store.App, project store.Project) {
	tr := tv.Trial
	if !tr.RunID.Valid {
		return
	}
	if s.kube != nil {
		jobName := jobRunName(app.Name, tr.RunID.Int64)
		if err := s.kube.DeleteJob(ctx, project.Namespace, jobName); err != nil {
			log.Printf("sweep %s: prune trial %s: delete job: %v", sw.ID, tr.ID, err)
		}
	}
	if err := s.st.FinishJobRun(tr.RunID.Int64, "failed", nil); err != nil {
		log.Printf("sweep %s: prune trial %s: finish run: %v", sw.ID, tr.ID, err)
	}
	var val *float64
	var step *int64
	if tv.Live.Found {
		v, st2 := tv.Live.Value, tv.Live.Step
		val, step = &v, &st2
	}
	if err := s.st.FinishTrial(tr.ID, "pruned", val, step); err != nil {
		log.Printf("sweep %s: prune trial %s: finish trial: %v", sw.ID, tr.ID, err)
	}
}

// sweepReconcile resolves every active sweep's "running" trials once at
// server start: a run that finished while the server was down is harvested
// now; a run row still "running" whose Job is gone (the process died before
// ever seeing it finish, and the Job itself was since cleaned up) is marked
// failed on both the run and the trial. Mirrors reconcile.go's deployment
// reconciliation. No-op without kube (can't tell "still running" from "job
// gone" without asking the cluster).
func (s *server) sweepReconcile(ctx context.Context) {
	if s.kube == nil {
		return
	}
	sweeps, err := s.st.ActiveSweeps()
	if err != nil {
		log.Printf("sweep reconcile: list active sweeps: %v", err)
		return
	}
	for _, sw := range sweeps {
		s.sweepReconcileOne(ctx, sw)
	}
}

func (s *server) sweepReconcileOne(ctx context.Context, sw store.Sweep) {
	trials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		log.Printf("sweep %s: reconcile: list trials: %v", sw.ID, err)
		return
	}
	app, project, ok := s.sweepAppProject(sw)
	if !ok {
		return
	}
	mlflowURL := s.sweepMLflowURLFn(app, project.Namespace)

	for _, tr := range trials {
		if tr.State != "running" || !tr.RunID.Valid {
			continue
		}
		run, err := s.st.GetJobRun(tr.RunID.Int64)
		if err != nil {
			log.Printf("sweep %s: reconcile trial %s: get run: %v", sw.ID, tr.ID, err)
			continue
		}
		if run.Status != "running" {
			// Finished while the server was down: harvest now.
			obs := s.trialMetric(ctx, sw, tr, app, project, run, mlflowURL)
			state := "done"
			if run.Status == "failed" {
				state = "failed"
			}
			var val *float64
			var step *int64
			if obs.Found {
				v, st2 := obs.Value, obs.Step
				val, step = &v, &st2
			}
			if err := s.st.FinishTrial(tr.ID, state, val, step); err != nil {
				log.Printf("sweep %s: reconcile finish trial %s: %v", sw.ID, tr.ID, err)
			}
			continue
		}

		jobName := jobRunName(app.Name, run.ID)
		exists, err := s.kube.JobExists(ctx, project.Namespace, jobName)
		if err != nil || exists {
			continue // still there (or a transient check error) — leave it, next tick tries again
		}
		// Run row says "running" but the Job is gone: orphaned by a crash.
		if e := s.st.FinishJobRun(run.ID, "failed", nil); e != nil {
			log.Printf("sweep %s: reconcile mark run %d failed: %v", sw.ID, run.ID, e)
		}
		if e := s.st.FinishTrial(tr.ID, "failed", nil, nil); e != nil {
			log.Printf("sweep %s: reconcile finish trial %s: %v", sw.ID, tr.ID, e)
		}
	}
}

// startSweepLoop reconciles every active sweep once (server restart) and
// then drives them every sweepLoopInterval, until ctx ends. No-op without
// kube (same guard as StartMonitor): startRun unconditionally touches the
// cluster, so the loop can't safely launch trials without one.
func (s *server) startSweepLoop(ctx context.Context) {
	if s.kube == nil {
		return
	}
	s.sweepReconcile(ctx)
	t := time.NewTicker(sweepLoopInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepTick(ctx)
		}
	}
}

// --- endpoints --------------------------------------------------------

// sweepCreateRequest is the JSON body of POST .../sweeps.
type sweepCreateRequest struct {
	ParamsYAML string `json:"params_yaml"`
	Metric     string `json:"metric"`
	Direction  string `json:"direction"`
	MaxTrials  int    `json:"max_trials"`
	Parallel   int    `json:"parallel"`
	EarlyStop  bool   `json:"early_stop"`
	Nodes      int    `json:"nodes"`
	Framework  string `json:"framework"`
}

// errBadSweepRequest wraps every validation failure inside startSweep (bad
// params.yaml, out-of-range metric/direction/bounds, unknown framework, a
// param key that wouldn't survive LUNCUR_PARAM_<UPPER> as an env name) so
// handleCreateSweep can map it to 400 without string-sniffing the error —
// same convention as errNotDeployed/errRunOverBudget in runs.go.
var errBadSweepRequest = errors.New("bad sweep request")

// paramKeyRe is the env-safe param key guard: LUNCUR_PARAM_<UPPER> must
// always be a legal env var name (see trialEnv). Enforced at create time so
// the orchestrator loop can assume it holds.
var paramKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// startSweep is handleCreateSweep's shared core: validate the request,
// parse+expand the param space, and create the sweep plus its all-pending
// trials in one store transaction. Kept separate from the handler so a
// future UI start form can share it (same convention as startRun in
// runs.go). Returns whether the param grid was truncated to max_trials.
func (s *server) startSweep(a store.App, req sweepCreateRequest, createdBy sql.NullInt64) (store.Sweep, []store.SweepTrial, bool, error) {
	if strings.TrimSpace(req.Metric) == "" {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: metric is required", errBadSweepRequest)
	}
	if req.Direction != "min" && req.Direction != "max" {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: direction must be min or max, got %q", errBadSweepRequest, req.Direction)
	}
	if req.MaxTrials < 1 || req.MaxTrials > 500 {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: max_trials must be 1..500, got %d", errBadSweepRequest, req.MaxTrials)
	}
	if req.Parallel < 1 || req.Parallel > 50 {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: parallel must be 1..50, got %d", errBadSweepRequest, req.Parallel)
	}
	if req.Framework != "" && !slices.Contains(render.TrainFrameworks, req.Framework) {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: unknown framework %q (valid: %s)",
			errBadSweepRequest, req.Framework, strings.Join(render.TrainFrameworks, ", "))
	}

	d, err := s.st.LatestDeployment(a.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.Status != "live") {
		return store.Sweep{}, nil, false, errNotDeployed
	}
	if err != nil {
		return store.Sweep{}, nil, false, fmt.Errorf("latest deployment: %w", err)
	}

	space, err := sweep.ParseParams([]byte(req.ParamsYAML))
	if err != nil {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: %v", errBadSweepRequest, err)
	}
	for key := range space {
		if !paramKeyRe.MatchString(key) {
			return store.Sweep{}, nil, false, fmt.Errorf("%w: param %q: keys must match %s",
				errBadSweepRequest, key, paramKeyRe.String())
		}
	}

	seed := time.Now().UnixNano()
	sets, truncated, err := sweep.Expand(space, req.MaxTrials, rand.New(rand.NewSource(seed)))
	if err != nil {
		return store.Sweep{}, nil, false, fmt.Errorf("%w: %v", errBadSweepRequest, err)
	}
	trialParams := make([]string, len(sets))
	for i, set := range sets {
		b, err := json.Marshal(set)
		if err != nil {
			return store.Sweep{}, nil, false, fmt.Errorf("encode trial params: %w", err)
		}
		trialParams[i] = string(b)
	}

	sw, trials, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: req.Metric, Direction: req.Direction,
		MaxTrials: req.MaxTrials, Parallel: req.Parallel, EarlyStop: req.EarlyStop,
		Nodes: req.Nodes, Framework: req.Framework, Seed: seed, CreatedBy: createdBy,
	}, trialParams)
	if err != nil {
		return store.Sweep{}, nil, false, fmt.Errorf("create sweep: %w", err)
	}
	return sw, trials, truncated, nil
}

// stopSweep is handleStopSweep's (and a future UI stop button's) shared
// core: a running sweep has its running trials' Jobs deleted and marked
// pruned — reusing sweepPruneTrial's pod-level/sweep-level truth split, just
// without a live metric observation to record — while pending trials are
// left alone (a stopped sweep never launches anything: the loop only drives
// status=running sweeps), and the sweep itself finishes "stopped". Callers
// must check sw.Status == "running" first; a non-running sweep is already
// the idempotent no-op case and this must not be called again for it.
func (s *server) stopSweep(ctx context.Context, sw store.Sweep, app store.App, project store.Project) error {
	trials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		return fmt.Errorf("list trials: %w", err)
	}
	for _, tr := range trials {
		if tr.State != "running" {
			continue
		}
		s.sweepPruneTrial(ctx, sw, trialView{Trial: tr}, app, project)
	}
	return s.st.FinishSweep(sw.ID, "stopped")
}

// sweepSummary computes per-state trial counts and the best (by sw.Direction)
// done trial with a recorded metric — shared by the list endpoint (counts +
// best only) and the detail endpoint (which also includes the full trial
// list).
func sweepSummary(sw store.Sweep, trials []store.SweepTrial) (counts map[string]int, best *store.SweepTrial) {
	counts = map[string]int{}
	for i := range trials {
		tr := trials[i]
		counts[tr.State]++
		if tr.State != "done" || !tr.MetricValue.Valid {
			continue
		}
		switch {
		case best == nil:
			b := tr
			best = &b
		case sw.Direction == "min" && tr.MetricValue.Float64 < best.MetricValue.Float64:
			b := tr
			best = &b
		case sw.Direction == "max" && tr.MetricValue.Float64 > best.MetricValue.Float64:
			b := tr
			best = &b
		}
	}
	return counts, best
}

func sweepBaseJSON(sw store.Sweep) map[string]any {
	out := map[string]any{
		"id":         sw.ID,
		"app_id":     sw.AppID,
		"metric":     sw.Metric,
		"direction":  sw.Direction,
		"max_trials": sw.MaxTrials,
		"parallel":   sw.Parallel,
		"early_stop": sw.EarlyStop,
		"nodes":      sw.Nodes,
		"status":     sw.Status,
		"created_at": sw.CreatedAt,
	}
	if sw.Framework != "" {
		out["framework"] = sw.Framework
	}
	if sw.Warning != "" {
		out["warning"] = sw.Warning
	}
	return out
}

// sweepListJSON is one row of GET .../sweeps: id/status/metric plus counts
// by state and the best trial's value (no per-trial detail).
func sweepListJSON(sw store.Sweep, trials []store.SweepTrial) map[string]any {
	out := sweepBaseJSON(sw)
	counts, best := sweepSummary(sw, trials)
	out["counts"] = counts
	if best != nil {
		out["best_trial_id"] = best.ID
		out["best_value"] = best.MetricValue.Float64
	}
	return out
}

func trialJSON(tr store.SweepTrial) map[string]any {
	out := map[string]any{
		"id":    tr.ID,
		"state": tr.State,
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(tr.ParamsJSON), &params); err == nil {
		out["params"] = params
	}
	if tr.RunID.Valid {
		out["run_id"] = tr.RunID.Int64
	}
	if tr.MetricValue.Valid {
		out["metric_value"] = tr.MetricValue.Float64
	}
	if tr.MetricStep.Valid {
		out["metric_step"] = tr.MetricStep.Int64
	}
	return out
}

// sweepJSON is the detail shape returned by create/get/stop: sweepListJSON's
// summary plus every trial (params, metric, state, run id).
func sweepJSON(sw store.Sweep, trials []store.SweepTrial) map[string]any {
	out := sweepListJSON(sw, trials)
	trialsOut := make([]map[string]any, 0, len(trials))
	for _, tr := range trials {
		trialsOut = append(trialsOut, trialJSON(tr))
	}
	out["trials"] = trialsOut
	return out
}

// handleCreateSweep starts a hyperparameter sweep over a kind=job app.
func (s *server) handleCreateSweep(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}

	var req sweepCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	var createdBy sql.NullInt64
	if u.ID != 0 {
		createdBy = sql.NullInt64{Int64: u.ID, Valid: true}
	}
	sw, trials, truncated, err := s.startSweep(a, req, createdBy)
	switch {
	case errors.Is(err, errNotDeployed):
		writeError(w, http.StatusConflict, "not_deployed", err.Error())
		return
	case errors.Is(err, errBadSweepRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case err != nil:
		log.Printf("create sweep: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	out := sweepJSON(sw, trials)
	if truncated {
		out["truncated"] = true
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *server) handleListSweeps(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	sweeps, err := s.st.ListSweeps(a.ID)
	if err != nil {
		log.Printf("list sweeps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(sweeps))
	for _, sw := range sweeps {
		trials, err := s.st.ListTrials(sw.ID)
		if err != nil {
			log.Printf("list trials for sweep %s: %v", sw.ID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		out = append(out, sweepListJSON(sw, trials))
	}
	writeJSON(w, http.StatusOK, out)
}

// requireSweep loads a sweep by id, verifying it belongs to app a.
func (s *server) requireSweep(w http.ResponseWriter, a store.App, id string) (store.Sweep, bool) {
	sw, err := s.st.GetSweep(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sw.AppID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such sweep")
		return store.Sweep{}, false
	}
	if err != nil {
		log.Printf("get sweep: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Sweep{}, false
	}
	return sw, true
}

func (s *server) handleGetSweep(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	sw, ok := s.requireSweep(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	trials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		log.Printf("list trials: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sweepJSON(sw, trials))
}

// handleStopSweep idempotently stops a sweep: a running sweep has its
// running trials killed and finishes "stopped"; an already-stopped (or
// done/failed) sweep is a 200 no-op that just reports current state.
func (s *server) handleStopSweep(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProjectWrite(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	sw, ok := s.requireSweep(w, a, r.PathValue("id"))
	if !ok {
		return
	}

	if sw.Status == "running" {
		if err := s.stopSweep(r.Context(), sw, a, p); err != nil {
			log.Printf("stop sweep %s: %v", sw.ID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		got, err := s.st.GetSweep(sw.ID)
		if err != nil {
			log.Printf("get sweep %s: %v", sw.ID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		sw = got
	}

	trials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		log.Printf("list trials: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sweepJSON(sw, trials))
}
