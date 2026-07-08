package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/addon"
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
