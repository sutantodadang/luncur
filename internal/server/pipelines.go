package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// pipeStepView pairs a step row with its compiled spec step and, for
// kind=app steps, its job run — nil while the step is pending, or for any
// non-app kind (kind=image's Job status is resolved by the engine's
// harvest directly against the row state; kind=deploy/scale/notify run
// synchronously and never sit in "running" across ticks).
type pipeStepView struct {
	Row  store.PipelineRunStep
	Spec pipeline.Step
	Run  *store.JobRun
}

// pipeActions is one tick's pure decision: which pending/retrying step rows
// to (re)launch, which pending rows to fail-fast skip, and whether the run
// as a whole has finished.
type pipeActions struct {
	Launch []string // step row IDs to (re)launch — includes retries
	Skip   []string // step row IDs to mark skipped (fail-fast downstream)
	Finish string   // "" = keep going; "done"|"failed" = finish the run
}

// decidePipelineRun is the pure per-tick decision core — no store/kube
// calls. Rules (spec §Native):
//  1. done/failed/skipped rows are inert (except rule 3 reads failed rows
//     to compute the skip set).
//  2. A pending row launches once every needs-step row is done.
//  3. A failed row transitively skips its downstream pending rows
//     (spec.Downstream) — fail-fast. A running app-kind row whose Run
//     reports failure and whose attempt is still under budget (attempt <
//     retries) relaunches instead of failing; once attempt reaches
//     retries the engine marks the row failed itself before the next call
//     to decide, so this core never needs to fail a row on its own.
//  4. Finish fires once no row is pending or running: "done" if every row
//     is done, else "failed" (skipped counts as not-done). A row this
//     tick's Skip targets is still "pending" in the input (the engine
//     hasn't applied the transition yet), so it keeps Finish at "" until
//     a later tick observes it already skipped.
func decidePipelineRun(spec pipeline.Spec, views []pipeStepView) pipeActions {
	byName := make(map[string]pipeStepView, len(views))
	for _, v := range views {
		byName[v.Spec.Name] = v
	}

	var actions pipeActions
	skip := make(map[string]bool, len(views))
	for _, v := range views {
		if v.Row.State != "failed" {
			continue
		}
		for _, dep := range spec.Downstream(v.Spec.Name) {
			dv, ok := byName[dep]
			if !ok || dv.Row.State != "pending" || skip[dv.Row.ID] {
				continue
			}
			skip[dv.Row.ID] = true
			actions.Skip = append(actions.Skip, dv.Row.ID)
		}
	}

	anyPendingOrRunning := false
	allDone := true
	for _, v := range views {
		switch v.Row.State {
		case "pending":
			anyPendingOrRunning = true
			allDone = false
			if !skip[v.Row.ID] && allNeedsDone(byName, v.Spec.Needs) {
				actions.Launch = append(actions.Launch, v.Row.ID)
			}
		case "running":
			anyPendingOrRunning = true
			allDone = false
			if v.Spec.Kind == "app" && v.Run != nil && v.Run.Status == "failed" && v.Row.Attempt < v.Spec.Retries {
				actions.Launch = append(actions.Launch, v.Row.ID)
			}
		case "done":
			// inert, and the only state that keeps allDone true.
		default: // failed, skipped
			allDone = false
		}
	}

	if !anyPendingOrRunning {
		if allDone {
			actions.Finish = "done"
		} else {
			actions.Finish = "failed"
		}
	}

	return actions
}

// allNeedsDone reports whether every named upstream step's row is "done".
func allNeedsDone(byName map[string]pipeStepView, needs []string) bool {
	for _, n := range needs {
		v, ok := byName[n]
		if !ok || v.Row.State != "done" {
			return false
		}
	}
	return true
}

// --- engine loop --------------------------------------------------------

// pipelineLoopInterval is startPipelineLoop's tick cadence — same lifecycle
// shape as startSweepLoop (sweeps.go).
const pipelineLoopInterval = 30 * time.Second

// startPipelineLoop reconciles every active run once (server restart) and
// then drives them every pipelineLoopInterval, until ctx ends. No-op without
// kube (same guard as StartMonitor/startSweepLoop): app steps unconditionally
// touch the cluster via startRun, and image/deploy/scale steps apply
// directly, so the loop can't safely drive anything without one.
func (s *server) startPipelineLoop(ctx context.Context) {
	if s.kube == nil {
		return
	}
	s.pipelineReconcile(ctx)
	t := time.NewTicker(pipelineLoopInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pipelineTick(ctx)
		}
	}
}

// pipelineTick drives one tick of every active pipeline run.
func (s *server) pipelineTick(ctx context.Context) {
	runs, err := s.st.ActivePipelineRuns()
	if err != nil {
		log.Printf("pipeline tick: list active runs: %v", err)
		return
	}
	for _, run := range runs {
		s.pipelineTickOne(ctx, run)
	}
}

// pipelineRunContext loads the state a run's tick/reconcile/stop all need:
// the pipeline row (for its project + name — ArtifactEnv needs the name,
// notify needs both), the project, and the compiled spec snapshot.
func (s *server) pipelineRunContext(run store.PipelineRun) (store.Pipeline, store.Project, pipeline.Spec, error) {
	pl, err := s.st.GetPipelineByID(run.PipelineID)
	if err != nil {
		return store.Pipeline{}, store.Project{}, pipeline.Spec{}, fmt.Errorf("get pipeline %s: %w", run.PipelineID, err)
	}
	project, err := s.st.GetProjectByID(pl.ProjectID)
	if err != nil {
		return store.Pipeline{}, store.Project{}, pipeline.Spec{}, fmt.Errorf("get project %d: %w", pl.ProjectID, err)
	}
	var spec pipeline.Spec
	if err := json.Unmarshal([]byte(run.SpecJSON), &spec); err != nil {
		return store.Pipeline{}, store.Project{}, pipeline.Spec{}, fmt.Errorf("unmarshal spec: %w", err)
	}
	return pl, project, spec, nil
}

// pipelineResolveApp looks up an app by name within the run's project,
// caching per tick (mirrors sweeps resolving its single app once per tick —
// a pipeline run can reference several apps, hence the cache instead of a
// single field).
func (s *server) pipelineResolveApp(project store.Project, name string, cache map[string]store.App) (store.App, bool) {
	if a, ok := cache[name]; ok {
		return a, true
	}
	a, err := s.st.GetApp(project.ID, name)
	if err != nil {
		return store.App{}, false
	}
	cache[name] = a
	return a, true
}

func (s *server) pipelineTickOne(ctx context.Context, run store.PipelineRun) {
	pl, project, spec, err := s.pipelineRunContext(run)
	if err != nil {
		log.Printf("pipeline run %s: %v", run.ID, err)
		return
	}
	rows, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		log.Printf("pipeline run %s: list steps: %v", run.ID, err)
		return
	}

	appCache := map[string]store.App{}
	views := make([]pipeStepView, 0, len(rows))
	viewByID := make(map[string]pipeStepView, len(rows))
	for _, row := range rows {
		st, ok := spec.Step(row.Name)
		if !ok {
			// spec_json is an immutable snapshot and rows are pre-expanded
			// from it at CreatePipelineRun time — a row without a matching
			// spec step should never happen. Skip defensively rather than
			// feed decidePipelineRun a zero-value Spec.
			log.Printf("pipeline run %s: step row %s (%q) has no matching spec step", run.ID, row.ID, row.Name)
			continue
		}
		v := pipeStepView{Row: row, Spec: st}
		if row.State == "running" {
			v = s.pipelineHarvestStep(ctx, run, pl, project, v, appCache)
		}
		views = append(views, v)
		viewByID[v.Row.ID] = v
	}

	actions := decidePipelineRun(spec, views)

	for _, id := range actions.Skip {
		if err := s.st.FinishStep(id, "skipped", "upstream failed"); err != nil {
			log.Printf("pipeline run %s: skip step %s: %v", run.ID, id, err)
		}
	}
	for _, id := range actions.Launch {
		if v, ok := viewByID[id]; ok {
			s.pipelineLaunchStep(ctx, run, pl, project, v, appCache)
		}
	}
	if actions.Finish != "" {
		if err := s.st.FinishPipelineRun(run.ID, actions.Finish); err != nil {
			log.Printf("pipeline run %s: finish run: %v", run.ID, err)
			return
		}
		s.notify(notifyEvent{
			Event:   "pipeline",
			Project: project.Name,
			App:     pl.Name,
			Message: fmt.Sprintf("pipeline %s run %s: %s", pl.Name, run.ID, actions.Finish),
		})
	}
}

// pipelineHarvestStep updates one "running" row's view for this tick.
//
// kind=app fetches its job_runs row: succeeded finishes the step done;
// failed with attempt exhausted finishes it failed; failed with attempts
// remaining leaves the row running but attaches Run so decidePipelineRun's
// pure core relaunches it (its retry rule is app-only — see decide's rule
// 3); still-running does nothing.
//
// kind=image polls its Job directly via Task 3's JobDone — decidePipelineRun
// has no image-retry path (Run is app-only, sourced from job_runs), so a
// failed image attempt with retries remaining is relaunched right here
// rather than through decide's Launch list.
//
// deploy/scale/notify steps execute synchronously in the Launch phase and
// never sit in "running" across ticks, so they never reach this function.
func (s *server) pipelineHarvestStep(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView, appCache map[string]store.App) pipeStepView {
	switch v.Spec.Kind {
	case "app":
		if !v.Row.JobRunID.Valid {
			return v // launched but the job_runs row isn't recorded yet (shouldn't happen)
		}
		jr, err := s.st.GetJobRun(v.Row.JobRunID.Int64)
		if err != nil {
			log.Printf("pipeline run %s: step %s: get job run %d: %v", run.ID, v.Row.Name, v.Row.JobRunID.Int64, err)
			return v
		}
		switch jr.Status {
		case "succeeded":
			if err := s.st.FinishStep(v.Row.ID, "done", "exit 0"); err != nil {
				log.Printf("pipeline run %s: step %s: finish done: %v", run.ID, v.Row.Name, err)
				return v
			}
			v.Row.State = "done"
		case "failed":
			if v.Row.Attempt >= v.Spec.Retries {
				detail := "run failed"
				if jr.ExitCode.Valid {
					detail = fmt.Sprintf("exit %d", jr.ExitCode.Int64)
				}
				if err := s.st.FinishStep(v.Row.ID, "failed", detail); err != nil {
					log.Printf("pipeline run %s: step %s: finish failed: %v", run.ID, v.Row.Name, err)
					return v
				}
				v.Row.State = "failed"
			} else {
				jrCopy := jr
				v.Run = &jrCopy
			}
		}
	case "image":
		name := render.PipelineStepJobName(run.ID, v.Row.Name, v.Row.Attempt)
		done, failed, err := s.kube.JobDone(ctx, project.Namespace, name)
		if err != nil {
			log.Printf("pipeline run %s: step %s: job status: %v", run.ID, v.Row.Name, err)
			return v
		}
		if !done {
			return v
		}
		if !failed {
			if err := s.st.FinishStep(v.Row.ID, "done", "exit 0"); err != nil {
				log.Printf("pipeline run %s: step %s: finish done: %v", run.ID, v.Row.Name, err)
				return v
			}
			v.Row.State = "done"
			return v
		}
		if v.Row.Attempt >= v.Spec.Retries {
			if err := s.st.FinishStep(v.Row.ID, "failed", "job failed"); err != nil {
				log.Printf("pipeline run %s: step %s: finish failed: %v", run.ID, v.Row.Name, err)
				return v
			}
			v.Row.State = "failed"
			return v
		}
		s.pipelineLaunchImage(ctx, run, pl, project, v, appCache)
	}
	return v
}

// pipelineLaunchStep dispatches a decided Launch (or an image-retry from
// pipelineHarvestStep) to its kind-specific handler.
func (s *server) pipelineLaunchStep(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView, appCache map[string]store.App) {
	switch v.Spec.Kind {
	case "app":
		s.pipelineLaunchApp(ctx, run, pl, project, v, appCache)
	case "image":
		s.pipelineLaunchImage(ctx, run, pl, project, v, appCache)
	case "deploy":
		s.pipelineRunDeploy(ctx, run, pl, project, v, appCache)
	case "scale":
		s.pipelineRunScale(ctx, run, pl, project, v, appCache)
	case "notify":
		s.pipelineRunNotify(run, pl, project, v)
	}
}

// pipelineStepEnv is the env every app/image step launch carries: the
// artifact/convention trio plus outputs/inputs from ArtifactEnv, with the
// step's own declared env overlaid on top (step env wins per
// pipeline.ArtifactEnv's doc comment).
func pipelineStepEnv(pl store.Pipeline, runID string, st pipeline.Step) map[string]string {
	env := pipeline.ArtifactEnv(pl.Name, runID, st)
	for k, v := range st.Env {
		env[k] = v
	}
	return env
}

// pipelineLaunchApp starts (or retries) an app-kind step via B1's startRun,
// sharing its GPU-budget pacing: errRunOverBudget leaves the row exactly as
// it is for a later tick to retry (pending row stays pending; a retry
// candidate stays running with its Run attached), exactly like a sweep trial
// that hits quota.
func (s *server) pipelineLaunchApp(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView, appCache map[string]store.App) {
	a, ok := s.pipelineResolveApp(project, v.Spec.App, appCache)
	if !ok || a.Kind != "job" {
		if err := s.st.FinishStep(v.Row.ID, "failed", fmt.Sprintf("app %q not found or not kind=job", v.Spec.App)); err != nil {
			log.Printf("pipeline run %s: step %s: finish failed: %v", run.ID, v.Row.Name, err)
		}
		return
	}

	jr, err := s.startRun(ctx, project, a, runOpts{Env: pipelineStepEnv(pl, run.ID, v.Spec)})
	switch {
	case errors.Is(err, errRunOverBudget):
		log.Printf("pipeline run %s: step %s over gpu budget this tick, left pending: %v", run.ID, v.Row.Name, err)
		return
	case err != nil:
		if e := s.st.FinishStep(v.Row.ID, "failed", err.Error()); e != nil {
			log.Printf("pipeline run %s: step %s: finish failed: %v", run.ID, v.Row.Name, e)
		}
		return
	}
	jrID := jr.ID
	if err := s.st.MarkStepRunning(v.Row.ID, &jrID, v.Row.Attempt+1); err != nil {
		log.Printf("pipeline run %s: step %s: mark running: %v", run.ID, v.Row.Name, err)
	}
}

// pipelineLaunchImage starts (or retries) an inline image-kind step: render
// the plain Job for the bumped attempt and apply it. Called both from the
// decided Launch list (first launch, row pending) and directly from
// pipelineHarvestStep (a retry of an already-running row — decidePipelineRun
// has no image-retry path, see pipelineHarvestStep's doc comment).
func (s *server) pipelineLaunchImage(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView, appCache map[string]store.App) {
	attempt := v.Row.Attempt + 1
	if err := s.st.MarkStepRunning(v.Row.ID, nil, attempt); err != nil {
		log.Printf("pipeline run %s: step %s: mark running: %v", run.ID, v.Row.Name, err)
		return
	}

	env := pipelineStepEnv(pl, run.ID, v.Spec)
	for k, val := range s.pipelineS3Env(project) {
		if _, taken := env[k]; !taken {
			env[k] = val
		}
	}
	objs := render.PipelineStepJob(project.Namespace, run.ID, v.Row.Name, attempt, v.Spec.Image, v.Spec.Command, env, v.Spec.GPU)

	err := s.kube.EnsureNamespace(ctx, project.Namespace)
	if err == nil {
		err = s.kube.Apply(ctx, project.Namespace, objs)
	}
	if err != nil {
		if e := s.st.FinishStep(v.Row.ID, "failed", err.Error()); e != nil {
			log.Printf("pipeline run %s: step %s: finish failed: %v", run.ID, v.Row.Name, e)
		}
	}
}

// pipelineS3Env returns a project's external S3 env for an inline image
// step, mirroring renderAppWithRun's opt-in S3 injection (sync.go) — except
// image steps have no app row (and so no InjectS3 flag) to opt in with, so
// this injects unconditionally whenever the project has S3 configured. Nil
// (silently — this must not fail the step) when the project has no S3
// configured or its keys can't be unsealed; the step's env just lacks the
// vars in that case.
func (s *server) pipelineS3Env(project store.Project) map[string]string {
	cfg, err := s.st.GetProjectS3(project.ID)
	if err != nil || s.sealer == nil {
		return nil
	}
	ak, err := s.sealer.Open(cfg.AccessKeyEnc)
	if err != nil {
		return nil
	}
	sk, err := s.sealer.Open(cfg.SecretKeyEnc)
	if err != nil {
		return nil
	}
	env := map[string]string{
		"LUNCUR_S3_ENDPOINT": cfg.Endpoint,
		"LUNCUR_S3_KEY":      string(ak),
		"LUNCUR_S3_SECRET":   string(sk),
		"LUNCUR_S3_BUCKET":   cfg.Bucket,
	}
	if cfg.Region != "" {
		env["LUNCUR_S3_REGION"] = cfg.Region
	}
	return env
}

// pipelineMarkActionRunning transitions a deploy/scale/notify step's row
// pending -> running before it executes: these actions run synchronously
// within one tick (never sitting in "running" across ticks — see
// pipeStepView's doc comment), but FinishStep only allows "done" from
// "running" (a step must have launched to succeed — store's FinishStep doc
// comment), so they still pass through MarkStepRunning first. Returns false
// (already logged) when the transition itself fails, in which case the
// caller must not attempt the action or finish the step this tick.
func (s *server) pipelineMarkActionRunning(run store.PipelineRun, v pipeStepView) bool {
	if err := s.st.MarkStepRunning(v.Row.ID, nil, v.Row.Attempt+1); err != nil {
		log.Printf("pipeline run %s: step %s: mark running: %v", run.ID, v.Row.Name, err)
		return false
	}
	return true
}

// pipelineRunDeploy is the "deploy" built-in action: redeploy the target
// app's current live image — the same row-create + apply core the rollback
// handler uses (rollback.go's `rollback`), just re-targeting the app's own
// latest live image instead of an earlier one (so no rolled_back_from
// lineage is recorded; this isn't a rollback, it's a redeploy/restart).
func (s *server) pipelineRunDeploy(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView, appCache map[string]store.App) {
	if !s.pipelineMarkActionRunning(run, v) {
		return
	}
	a, ok := s.pipelineResolveApp(project, v.Spec.Deploy, appCache)
	if !ok {
		s.finishPipelineStep(run, v, "failed", fmt.Sprintf("app %q not found", v.Spec.Deploy))
		return
	}
	live, err := s.st.LatestDeployment(a.ID)
	if err != nil || live.Status != "live" || live.ImageRef == "" {
		s.finishPipelineStep(run, v, "failed", fmt.Sprintf("app %q has no live deployment to redeploy", v.Spec.Deploy))
		return
	}
	d, err := s.st.CreateDeployment(a.ID, "deploying", live.ImageRef, 0)
	if err != nil {
		s.finishPipelineStep(run, v, "failed", err.Error())
		return
	}
	if err := s.applyImageDeploy(ctx, project, a, d, live.ImageRef); err != nil {
		s.finishPipelineStep(run, v, "failed", err.Error())
		return
	}
	s.finishPipelineStep(run, v, "done", fmt.Sprintf("deployed %s", live.ImageRef))
}

// pipelineRunScale is the "scale" built-in action: set an app's replica
// count via the shared scaleApp core (apps.go).
func (s *server) pipelineRunScale(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView, appCache map[string]store.App) {
	if !s.pipelineMarkActionRunning(run, v) {
		return
	}
	a, ok := s.pipelineResolveApp(project, v.Spec.Scale.App, appCache)
	if !ok {
		s.finishPipelineStep(run, v, "failed", fmt.Sprintf("app %q not found", v.Spec.Scale.App))
		return
	}
	replicas := v.Spec.Scale.Replicas
	if _, err := s.scaleApp(ctx, project, a, scaleChange{Replicas: &replicas}); err != nil {
		s.finishPipelineStep(run, v, "failed", err.Error())
		return
	}
	s.finishPipelineStep(run, v, "done", fmt.Sprintf("scaled %s to %d replicas", a.Name, replicas))
}

// pipelineRunNotify is the "notify" built-in action: fire a best-effort
// webhook notification (event "pipeline") carrying the step's configured
// message, then finish the step done — notify.notify is fire-and-forget by
// design (notify.go), so a notify-action step can't itself fail.
func (s *server) pipelineRunNotify(run store.PipelineRun, pl store.Pipeline, project store.Project, v pipeStepView) {
	if !s.pipelineMarkActionRunning(run, v) {
		return
	}
	s.notify(notifyEvent{Event: "pipeline", Project: project.Name, App: pl.Name, Message: v.Spec.Notify})
	s.finishPipelineStep(run, v, "done", "notified")
}

func (s *server) finishPipelineStep(run store.PipelineRun, v pipeStepView, state, detail string) {
	if err := s.st.FinishStep(v.Row.ID, state, detail); err != nil {
		log.Printf("pipeline run %s: step %s: finish %s: %v", run.ID, v.Row.Name, state, err)
	}
}

// stopPipelineRun is the shared core for the API stop endpoint (Task 6) and
// a future UI stop button: every running step's Job (image) or job_runs row
// (app) is torn down and the row marked failed(detail "stopped"), every
// pending row is marked skipped(detail "stopped"), and the run itself
// finishes "stopped". Mirrors stopSweep's pod-level/sweep-level truth split.
// Callers must check run.Status == "running" first — a non-running run is
// already the idempotent no-op case (B2 stopSweep convention) and this must
// not be called again for it.
func (s *server) stopPipelineRun(ctx context.Context, run store.PipelineRun) error {
	_, project, spec, err := s.pipelineRunContext(run)
	if err != nil {
		return err
	}
	rows, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}

	appCache := map[string]store.App{}
	for _, row := range rows {
		switch row.State {
		case "running":
			st, ok := spec.Step(row.Name)
			kind := row.Kind
			if ok {
				kind = st.Kind
			}
			switch kind {
			case "app":
				if row.JobRunID.Valid {
					if a, aok := s.pipelineResolveApp(project, st.App, appCache); aok && s.kube != nil {
						if err := s.kube.DeleteJob(ctx, project.Namespace, jobRunName(a.Name, row.JobRunID.Int64)); err != nil {
							log.Printf("pipeline run %s: stop: delete job for step %s: %v", run.ID, row.Name, err)
						}
					}
					if err := s.st.FinishJobRun(row.JobRunID.Int64, "failed", nil); err != nil {
						log.Printf("pipeline run %s: stop: finish job run %d: %v", run.ID, row.JobRunID.Int64, err)
					}
				}
			case "image":
				if s.kube != nil {
					name := render.PipelineStepJobName(run.ID, row.Name, row.Attempt)
					if err := s.kube.DeleteJob(ctx, project.Namespace, name); err != nil {
						log.Printf("pipeline run %s: stop: delete job for step %s: %v", run.ID, row.Name, err)
					}
				}
			}
			if err := s.st.FinishStep(row.ID, "failed", "stopped"); err != nil {
				log.Printf("pipeline run %s: stop: finish step %s: %v", run.ID, row.Name, err)
			}
		case "pending":
			if err := s.st.FinishStep(row.ID, "skipped", "stopped"); err != nil {
				log.Printf("pipeline run %s: stop: skip step %s: %v", run.ID, row.Name, err)
			}
		}
	}
	return s.st.FinishPipelineRun(run.ID, "stopped")
}

// pipelineReconcile resolves every active run's "running" steps once at
// server start: a step whose job_runs row or Job finished while the server
// was down is harvested now (same terminal mapping as a normal tick's
// harvest); a step whose Job vanished entirely (process died before ever
// seeing it finish, and the Job itself was since cleaned up) is marked
// failed. Mirrors sweepReconcile. Callers must guard s.kube == nil
// (startPipelineLoop does).
func (s *server) pipelineReconcile(ctx context.Context) {
	runs, err := s.st.ActivePipelineRuns()
	if err != nil {
		log.Printf("pipeline reconcile: list active runs: %v", err)
		return
	}
	for _, run := range runs {
		s.pipelineReconcileOne(ctx, run)
	}
}

func (s *server) pipelineReconcileOne(ctx context.Context, run store.PipelineRun) {
	_, project, spec, err := s.pipelineRunContext(run)
	if err != nil {
		log.Printf("pipeline run %s: reconcile: %v", run.ID, err)
		return
	}
	rows, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		log.Printf("pipeline run %s: reconcile: list steps: %v", run.ID, err)
		return
	}

	appCache := map[string]store.App{}
	for _, row := range rows {
		if row.State != "running" {
			continue
		}
		st, ok := spec.Step(row.Name)
		if !ok {
			continue
		}
		switch st.Kind {
		case "app":
			if !row.JobRunID.Valid {
				continue
			}
			jr, err := s.st.GetJobRun(row.JobRunID.Int64)
			if err != nil {
				log.Printf("pipeline run %s: reconcile: get job run %d: %v", run.ID, row.JobRunID.Int64, err)
				continue
			}
			if jr.Status != "running" {
				state, detail := "done", "exit 0"
				if jr.Status == "failed" {
					state, detail = "failed", "run failed"
					if jr.ExitCode.Valid {
						detail = fmt.Sprintf("exit %d", jr.ExitCode.Int64)
					}
				}
				if err := s.st.FinishStep(row.ID, state, detail); err != nil {
					log.Printf("pipeline run %s: reconcile: finish step %s: %v", run.ID, row.Name, err)
				}
				continue
			}
			a, aok := s.pipelineResolveApp(project, st.App, appCache)
			if !aok {
				continue
			}
			exists, err := s.kube.JobExists(ctx, project.Namespace, jobRunName(a.Name, jr.ID))
			if err != nil || exists {
				continue // still there (or a transient check error) — leave it, next tick tries again
			}
			if e := s.st.FinishJobRun(jr.ID, "failed", nil); e != nil {
				log.Printf("pipeline run %s: reconcile: finish job run %d: %v", run.ID, jr.ID, e)
			}
			if e := s.st.FinishStep(row.ID, "failed", "job missing after restart"); e != nil {
				log.Printf("pipeline run %s: reconcile: finish step %s: %v", run.ID, row.Name, e)
			}
		case "image":
			name := render.PipelineStepJobName(run.ID, row.Name, row.Attempt)
			exists, err := s.kube.JobExists(ctx, project.Namespace, name)
			if err != nil || exists {
				continue // still there (or transient) — a normal tick's JobDone harvest takes it from here
			}
			if err := s.st.FinishStep(row.ID, "failed", "job missing after restart"); err != nil {
				log.Printf("pipeline run %s: reconcile: finish step %s: %v", run.ID, row.Name, err)
			}
		}
	}
}
