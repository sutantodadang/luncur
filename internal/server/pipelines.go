package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/sutantodadang/luncur/internal/cronexpr"
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

// pipelineTick drives one tick of every active pipeline run, firing any due
// cron-scheduled pipelines first (firePipelineCrons) so a run it just
// created gets its root steps driven in the same tick, same as a manual
// trigger's inline pipelineTick (startPipelineRun's doc comment).
func (s *server) pipelineTick(ctx context.Context) {
	s.firePipelineCrons(ctx)

	runs, err := s.st.ActivePipelineRuns()
	if err != nil {
		log.Printf("pipeline tick: list active runs: %v", err)
		return
	}
	for _, run := range runs {
		s.pipelineTickOne(ctx, run)
	}
}

// pipelineStartedAtLayout is the Go time layout matching SQLite's
// datetime('now') column format, used to parse PipelineRun.StartedAt for the
// cron same-minute dedupe check below.
const pipelineStartedAtLayout = "2006-01-02 15:04:05"

// firePipelineCrons starts a run for every cron-scheduled pipeline that's
// due this minute, called at the top of pipelineTick before run driving.
// Never crashes the loop: an unparseable cron (creation validates this, so
// it's defensive only), an over-budget/engine-unavailable start, or any
// other startPipelineRun failure is logged and skipped rather than
// propagated.
func (s *server) firePipelineCrons(ctx context.Context) {
	pipelines, err := s.st.CronPipelines()
	if err != nil {
		log.Printf("pipeline cron: list cron pipelines: %v", err)
		return
	}
	now := s.nowFn().UTC().Truncate(time.Minute)
	for _, pl := range pipelines {
		sched, err := cronexpr.Parse(pl.Cron)
		if err != nil {
			log.Printf("pipeline %s: cron %q unparseable, skipped: %v", pl.Name, pl.Cron, err)
			continue
		}
		if !sched.Matches(now) {
			continue
		}

		runs, err := s.st.ListPipelineRuns(pl.ID)
		if err != nil {
			log.Printf("pipeline %s: cron: list runs: %v", pl.Name, err)
			continue
		}
		if len(runs) > 0 {
			last := runs[0] // ListPipelineRuns is newest first
			if last.Status == "running" {
				log.Printf("pipeline %s: cron skipped, previous run still running", pl.Name)
				continue
			}
			if startedAt, err := time.Parse(pipelineStartedAtLayout, last.StartedAt); err == nil &&
				startedAt.UTC().Truncate(time.Minute).Equal(now) {
				continue // already fired this minute
			}
		}

		if _, _, err := s.startPipelineRun(ctx, pl, "cron"); err != nil {
			log.Printf("pipeline %s: cron: start run: %v", pl.Name, err)
		}
	}
}

// pipelineRunContext loads the state a run's tick/reconcile/stop all need:
// the pipeline row (for its project + name — ArtifactEnv needs the name,
// notify needs both), the project, the compiled spec snapshot, and the
// run's own engine ("" or "native" = native, "argo" = argo). engine comes
// from decodePipelineRunSpec, not pl.Engine — the pipeline's engine setting
// can change after a run starts, but the run itself must keep driving on
// whichever engine actually launched it.
func (s *server) pipelineRunContext(run store.PipelineRun) (store.Pipeline, store.Project, pipeline.Spec, string, error) {
	pl, err := s.st.GetPipelineByID(run.PipelineID)
	if err != nil {
		return store.Pipeline{}, store.Project{}, pipeline.Spec{}, "", fmt.Errorf("get pipeline %s: %w", run.PipelineID, err)
	}
	project, err := s.st.GetProjectByID(pl.ProjectID)
	if err != nil {
		return store.Pipeline{}, store.Project{}, pipeline.Spec{}, "", fmt.Errorf("get project %d: %w", pl.ProjectID, err)
	}
	spec, engine, err := decodePipelineRunSpec(run.SpecJSON)
	if err != nil {
		return store.Pipeline{}, store.Project{}, pipeline.Spec{}, "", fmt.Errorf("unmarshal spec: %w", err)
	}
	return pl, project, spec, engine, nil
}

// decodePipelineRunSpec parses a run's spec_json into its compiled spec and
// declared engine. Native runs (and every run created before C3) store a
// bare pipeline.Spec — engine=="" there means native. C3's argo runs wrap
// it as {"engine":"argo","spec":{...}} so the run's engine survives even if
// the pipeline's own engine setting changes later (startPipelineRun's doc
// comment). Detected by probing for a top-level "engine" key rather than
// trying-and-falling-back: a bare Spec has no "engine" key, but unmarshaling
// it into the envelope struct wouldn't error either (its "spec" key would
// just be missing) — it would silently succeed with an empty Steps slice.
func decodePipelineRunSpec(raw string) (pipeline.Spec, string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return pipeline.Spec{}, "", err
	}
	if _, ok := probe["engine"]; ok {
		var envelope struct {
			Engine string        `json:"engine"`
			Spec   pipeline.Spec `json:"spec"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return pipeline.Spec{}, "", err
		}
		return envelope.Spec, envelope.Engine, nil
	}
	var spec pipeline.Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return pipeline.Spec{}, "", err
	}
	return spec, "", nil
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
	pl, project, spec, engine, err := s.pipelineRunContext(run)
	if err != nil {
		log.Printf("pipeline run %s: %v", run.ID, err)
		return
	}
	if engine == "argo" {
		s.pipelineTickArgoRun(ctx, run, pl, project, spec)
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

	err := s.ensureProjectNamespace(ctx, project.Namespace)
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
	_, project, spec, engine, err := s.pipelineRunContext(run)
	if err != nil {
		return err
	}
	if engine == "argo" && s.kube != nil {
		if err := s.kube.DeleteWorkflow(ctx, project.Namespace, argoWorkflowName(run.ID)); err != nil {
			log.Printf("pipeline run %s: stop: delete workflow: %v", run.ID, err)
		}
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
	_, project, spec, engine, err := s.pipelineRunContext(run)
	if err != nil {
		log.Printf("pipeline run %s: reconcile: %v", run.ID, err)
		return
	}
	if engine == "argo" {
		// Argo compute rows have no native job_runs/Job to check for
		// survival — this reconcile pass understands only those. The
		// regular tick loop's GetWorkflow call correctly harvests an argo
		// run's true state once the loop resumes; nothing to do here.
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

// --- endpoints --------------------------------------------------------

// errBadPipelineRequest wraps every validation failure inside
// createPipeline/updatePipeline/startPipelineRun that isn't an app-ref
// problem (bad yaml, out-of-range engine value) so the handlers can map it to
// 400 bad_request without string-sniffing the error — same convention as
// errBadSweepRequest in sweeps.go.
var errBadPipelineRequest = errors.New("bad pipeline request")

// errPipelineAppMismatch is createPipeline/updatePipeline/startPipelineRun's
// sentinel for an "app:" step naming an app that doesn't exist or isn't
// kind=job — handlers map it to 400 kind_mismatch (mirrors requireJobApp's
// error code for the same underlying problem).
var errPipelineAppMismatch = errors.New("pipeline app mismatch")

// errPipelineEngineUnavailable was startPipelineRun's sentinel for
// engine="argo" while C3 hadn't shipped yet. The argo engine now has a real
// start path (startArgoPipelineRun); its own preflight failure is
// errArgoNotInstalled instead. Kept (with its response-mapping cases below)
// as a stable 400 engine_unavailable code in case a future engine value is
// accepted-but-not-yet-runnable the same way argo once was.
var errPipelineEngineUnavailable = errors.New("pipeline engine unavailable")

// errArgoNotInstalled is startPipelineRun's sentinel for an argo-engine run
// whose cluster doesn't have the Argo Workflows CRD installed (or has no
// kube client configured at all) — handlers map it to 400
// argo_not_installed, naming the fix (`luncur argo install`) in the message.
var errArgoNotInstalled = errors.New("argo workflows engine not installed")

// validPipelineEngine reports whether v is a legal Pipeline.Engine value:
// ""  follows the pipeline_engine setting (else "native"), "native"/"argo"
// pin the run's engine explicitly.
func validPipelineEngine(v string) bool {
	return v == "" || v == "native" || v == "argo"
}

// validatePipelineAppRefs checks every step's app reference against the
// store: "app:" steps must name an existing kind=job app in the project
// (errPipelineAppMismatch); "deploy:"/"scale.app:" targets must name an
// existing app of any kind (errBadPipelineRequest — this is an existence
// check, not a kind check). Structural validation (DAG shape, field
// legality) is pipeline.Compile's job; this only checks what Compile can't
// see — the store.
func (s *server) validatePipelineAppRefs(p store.Project, spec pipeline.Spec) error {
	for _, st := range spec.Steps {
		switch st.Kind {
		case "app":
			a, err := s.st.GetApp(p.ID, st.App)
			if err != nil || a.Kind != "job" {
				return fmt.Errorf("%w: step %q: app %q not found or not kind=job", errPipelineAppMismatch, st.Name, st.App)
			}
		case "deploy":
			if _, err := s.st.GetApp(p.ID, st.Deploy); err != nil {
				return fmt.Errorf("%w: step %q: app %q not found", errBadPipelineRequest, st.Name, st.Deploy)
			}
		case "scale":
			if _, err := s.st.GetApp(p.ID, st.Scale.App); err != nil {
				return fmt.Errorf("%w: step %q: app %q not found", errBadPipelineRequest, st.Name, st.Scale.App)
			}
		}
	}
	return nil
}

// validatePipelineCron rejects a non-empty cron that fails cronexpr.Parse,
// naming the "cron" field in the error (400 bad_request via
// errBadPipelineRequest, same convention as engine/yaml validation). An
// empty cron is always valid — it just means "no schedule".
func validatePipelineCron(cron string) error {
	if cron == "" {
		return nil
	}
	if _, err := cronexpr.Parse(cron); err != nil {
		return fmt.Errorf("%w: cron: %v", errBadPipelineRequest, err)
	}
	return nil
}

// createPipeline is handleCreatePipeline's shared core: compile+validate the
// yaml, validate cron (if set), check every app reference against the store,
// then persist. Kept separate from the handler so a future UI create form
// can share it (same convention as startSweep/startRun).
func (s *server) createPipeline(p store.Project, name, yamlStr, engine, cron string, createdBy sql.NullInt64) (store.Pipeline, error) {
	if !validPipelineEngine(engine) {
		return store.Pipeline{}, fmt.Errorf("%w: engine must be \"\", \"native\", or \"argo\", got %q", errBadPipelineRequest, engine)
	}
	if err := validatePipelineCron(cron); err != nil {
		return store.Pipeline{}, err
	}
	spec, err := pipeline.Compile([]byte(yamlStr))
	if err != nil {
		return store.Pipeline{}, fmt.Errorf("%w: %v", errBadPipelineRequest, err)
	}
	if err := s.validatePipelineAppRefs(p, spec); err != nil {
		return store.Pipeline{}, err
	}
	return s.st.CreatePipeline(store.Pipeline{
		ProjectID: p.ID,
		Name:      name,
		YAML:      yamlStr,
		Cron:      cron,
		Engine:    engine,
		CreatedBy: createdBy,
	})
}

// updatePipeline is handleUpdatePipeline's shared core: yaml/engine/cron are
// all optional in the request (nil = keep current value). Re-validates the
// effective yaml/engine/cron exactly like createPipeline, since an update can
// introduce the same kinds of problems a create can.
func (s *server) updatePipeline(p store.Project, pl store.Pipeline, yamlPtr, enginePtr, cronPtr *string) (store.Pipeline, error) {
	yamlStr := pl.YAML
	if yamlPtr != nil {
		yamlStr = *yamlPtr
	}
	engine := pl.Engine
	if enginePtr != nil {
		engine = *enginePtr
	}
	cron := pl.Cron
	if cronPtr != nil {
		cron = *cronPtr
	}
	if !validPipelineEngine(engine) {
		return store.Pipeline{}, fmt.Errorf("%w: engine must be \"\", \"native\", or \"argo\", got %q", errBadPipelineRequest, engine)
	}
	if err := validatePipelineCron(cron); err != nil {
		return store.Pipeline{}, err
	}
	spec, err := pipeline.Compile([]byte(yamlStr))
	if err != nil {
		return store.Pipeline{}, fmt.Errorf("%w: %v", errBadPipelineRequest, err)
	}
	if err := s.validatePipelineAppRefs(p, spec); err != nil {
		return store.Pipeline{}, err
	}
	if err := s.st.UpdatePipeline(pl.ID, yamlStr, cron, engine); err != nil {
		return store.Pipeline{}, err
	}
	return s.st.GetPipelineByID(pl.ID)
}

// startPipelineRun is the shared run-start core for every trigger (manual:
// handleCreatePipelineRun, cron: firePipelineCrons, webhook: C2's
// handlePipelineWebhookTrigger): resolve the run's engine (pipeline.Engine,
// else the pipeline_engine setting, else "native"), re-compile the stored
// yaml (safety net — it was validated at write time, but re-validate rather
// than trust a stale snapshot), re-check app references (an app referenced
// at create time may have been deleted/changed kind since), then create the
// run with its pre-expanded pending step rows in topo order, recording
// trigger (manual|cron|webhook). An argo-engine run additionally: preflights
// (CRD installed, actions terminal, static whole-DAG GPU budget, every
// compute step's image resolvable — argoPreflight) before any state is
// created, then after the run row exists (so its runID is known) compiles
// and applies the run's Workflow CR and marks every compute step's row
// running immediately (Argo owns their lifecycle from here). The run's
// engine is recorded in spec_json itself (decodePipelineRunSpec's envelope)
// so it survives even if the pipeline's own engine setting changes later.
// Also fires one immediate pipelineTick so the run's root steps launch
// instantly instead of waiting up to pipelineLoopInterval for the next tick
// — for a cron-triggered call this re-enters pipelineTick/firePipelineCrons
// one level deep; the nested firePipelineCrons pass sees this run already
// "running" and skips it (Forbid concurrency check), so it terminates.
func (s *server) startPipelineRun(ctx context.Context, pl store.Pipeline, trigger string) (store.PipelineRun, []store.PipelineRunStep, error) {
	engine := pl.Engine
	if engine == "" {
		if v, err := s.st.GetSetting("pipeline_engine"); err == nil {
			engine = v
		}
	}
	if engine == "" {
		engine = "native"
	}

	spec, err := pipeline.Compile([]byte(pl.YAML))
	if err != nil {
		return store.PipelineRun{}, nil, fmt.Errorf("%w: %v", errBadPipelineRequest, err)
	}
	project, err := s.st.GetProjectByID(pl.ProjectID)
	if err != nil {
		return store.PipelineRun{}, nil, fmt.Errorf("get project %d: %w", pl.ProjectID, err)
	}
	if err := s.validatePipelineAppRefs(project, spec); err != nil {
		return store.PipelineRun{}, nil, err
	}

	var resolved map[string]argoResolvedStep
	if engine == "argo" {
		resolved, err = s.argoPreflight(ctx, project, spec)
		if err != nil {
			return store.PipelineRun{}, nil, err
		}
	}

	var specJSON []byte
	if engine == "argo" {
		b, err := json.Marshal(struct {
			Engine string        `json:"engine"`
			Spec   pipeline.Spec `json:"spec"`
		}{Engine: "argo", Spec: spec})
		if err != nil {
			return store.PipelineRun{}, nil, fmt.Errorf("encode spec: %w", err)
		}
		specJSON = b
	} else {
		b, err := json.Marshal(spec)
		if err != nil {
			return store.PipelineRun{}, nil, fmt.Errorf("encode spec: %w", err)
		}
		specJSON = b
	}
	stepNamesKinds := make([][2]string, len(spec.Steps))
	for i, st := range spec.Steps {
		stepNamesKinds[i] = [2]string{st.Name, st.Kind}
	}

	run, steps, err := s.st.CreatePipelineRun(store.PipelineRun{
		PipelineID: pl.ID,
		SpecJSON:   string(specJSON),
		Trigger:    trigger,
	}, stepNamesKinds)
	if err != nil {
		return store.PipelineRun{}, nil, fmt.Errorf("create run: %w", err)
	}

	if engine == "argo" {
		computeSteps := make([]argoComputeStep, 0, len(spec.Steps))
		for _, st := range spec.Steps {
			if st.Kind != "app" && st.Kind != "image" {
				continue
			}
			r := resolved[st.Name]
			// pipeline.Compile forbids gpu on app-kind steps (image-only
			// field); buildWorkflowCR reads Step.GPU directly, so the
			// resolved app-derived value (argoPreflight) is stamped onto
			// this copy of the compiled step.
			stepForCR := st
			if stepForCR.Kind == "app" {
				stepForCR.GPU = r.GPU
			}
			computeSteps = append(computeSteps, argoComputeStep{
				Step: stepForCR, Image: r.Image, Command: r.Command,
				Env: pipelineStepEnv(pl, run.ID, st),
			})
		}
		obj := buildWorkflowCR(project.Namespace, run.ID, computeSteps)
		applyErr := s.ensureProjectNamespace(ctx, project.Namespace)
		if applyErr == nil {
			applyErr = s.kube.Apply(ctx, project.Namespace, []render.Object{obj})
		}
		if applyErr != nil {
			if e := s.st.FinishPipelineRun(run.ID, "failed"); e != nil {
				log.Printf("pipeline run %s: finish failed run after apply error: %v", run.ID, e)
			}
			return store.PipelineRun{}, nil, fmt.Errorf("%w: apply workflow: %v", errRunStartFailed, applyErr)
		}
		byName := make(map[string]string, len(steps))
		for _, st := range steps {
			byName[st.Name] = st.ID
		}
		for _, cs := range computeSteps {
			if id, ok := byName[cs.Step.Name]; ok {
				if err := s.st.MarkStepRunning(id, nil, 0); err != nil {
					log.Printf("pipeline run %s: mark step %s running: %v", run.ID, cs.Step.Name, err)
				}
			}
		}
	}

	s.pipelineTick(ctx)

	got, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		return run, steps, nil // launched fine; report the pre-tick snapshot rather than fail the request
	}
	gotSteps, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		return got, steps, nil
	}
	return got, gotSteps, nil
}

// pipelineBaseJSON is the field set every pipeline response shares.
func pipelineBaseJSON(pl store.Pipeline) map[string]any {
	return map[string]any{
		"id":         pl.ID,
		"name":       pl.Name,
		"engine":     pl.Engine,
		"cron":       pl.Cron, // "" when unscheduled; CLI's `pipeline ls` needs this on every row, not just detail
		"created_at": pl.CreatedAt,
	}
}

// pipelineListJSON is one row of GET .../pipelines: base fields (including
// cron, for the CLI's ls CRON column) plus the newest run's
// id/status/started_at (nil when the pipeline has never run).
func pipelineListJSON(pl store.Pipeline, lastRun *store.PipelineRun) map[string]any {
	out := pipelineBaseJSON(pl)
	if lastRun != nil {
		out["last_run"] = map[string]any{
			"id":         lastRun.ID,
			"status":     lastRun.Status,
			"started_at": lastRun.StartedAt,
		}
	} else {
		out["last_run"] = nil
	}
	return out
}

// pipelineDetailJSON is the shape returned by create/update/get: base fields
// plus the raw yaml.
func pipelineDetailJSON(pl store.Pipeline) map[string]any {
	out := pipelineBaseJSON(pl)
	out["yaml"] = pl.YAML
	return out
}

// pipelineRunBaseJSON is the field set every run response shares.
func pipelineRunBaseJSON(run store.PipelineRun) map[string]any {
	out := map[string]any{
		"id":          run.ID,
		"pipeline_id": run.PipelineID,
		"status":      run.Status,
		"trigger":     run.Trigger,
		"started_at":  run.StartedAt,
	}
	if run.Warning != "" {
		out["warning"] = run.Warning
	}
	if run.FinishedAt.Valid {
		out["finished_at"] = run.FinishedAt.String
	}
	return out
}

// pipelineStepJSON renders one run step row.
func pipelineStepJSON(st store.PipelineRunStep) map[string]any {
	out := map[string]any{
		"name":    st.Name,
		"kind":    st.Kind,
		"state":   st.State,
		"attempt": st.Attempt,
		"detail":  st.Detail,
	}
	if st.JobRunID.Valid {
		out["job_run_id"] = st.JobRunID.Int64
	}
	if st.StartedAt.Valid {
		out["started_at"] = st.StartedAt.String
	}
	if st.FinishedAt.Valid {
		out["finished_at"] = st.FinishedAt.String
	}
	return out
}

// pipelineRunDetailJSON is the shape returned by run create/get/stop: base
// run fields plus every step, in row (topo) order.
func pipelineRunDetailJSON(run store.PipelineRun, steps []store.PipelineRunStep) map[string]any {
	out := pipelineRunBaseJSON(run)
	stepsOut := make([]map[string]any, 0, len(steps))
	for _, st := range steps {
		stepsOut = append(stepsOut, pipelineStepJSON(st))
	}
	out["steps"] = stepsOut
	return out
}

// requirePipeline loads a pipeline by its project-scoped name, 404ing when
// it doesn't exist in this project.
func (s *server) requirePipeline(w http.ResponseWriter, p store.Project, name string) (store.Pipeline, bool) {
	pl, err := s.st.GetPipeline(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such pipeline")
		return store.Pipeline{}, false
	}
	if err != nil {
		log.Printf("get pipeline: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Pipeline{}, false
	}
	return pl, true
}

// requirePipelineRun loads a run by id, verifying it belongs to pipeline pl.
func (s *server) requirePipelineRun(w http.ResponseWriter, pl store.Pipeline, id string) (store.PipelineRun, bool) {
	run, err := s.st.GetPipelineRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.PipelineID != pl.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such run")
		return store.PipelineRun{}, false
	}
	if err != nil {
		log.Printf("get pipeline run: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.PipelineRun{}, false
	}
	return run, true
}

// writePipelineRequestError maps createPipeline/updatePipeline/
// startPipelineRun's sentinel errors to their HTTP status/code, returning
// true if it wrote a response (the caller must return immediately).
func writePipelineRequestError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errPipelineEngineUnavailable):
		writeError(w, http.StatusBadRequest, "engine_unavailable", err.Error())
	case errors.Is(err, errArgoNotInstalled):
		writeError(w, http.StatusBadRequest, "argo_not_installed", err.Error())
	case errors.Is(err, errPipelineAppMismatch):
		writeError(w, http.StatusBadRequest, "kind_mismatch", err.Error())
	case errors.Is(err, errBadPipelineRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		return false
	}
	return true
}

// handleCreatePipeline creates a pipeline from a compiled+validated
// pipeline.yaml.
func (s *server) handleCreatePipeline(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Name   string `json:"name"`
		YAML   string `json:"yaml"`
		Engine string `json:"engine"`
		Cron   string `json:"cron"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	var createdBy sql.NullInt64
	if u.ID != 0 {
		createdBy = sql.NullInt64{Int64: u.ID, Valid: true}
	}
	pl, err := s.createPipeline(p, req.Name, req.YAML, req.Engine, req.Cron, createdBy)
	if err != nil {
		if writePipelineRequestError(w, err) {
			return
		}
		log.Printf("create pipeline: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, pipelineDetailJSON(pl))
}

// handleUpdatePipeline replaces a pipeline's yaml/engine/cron (all optional;
// omitted fields keep their current value). An empty-string cron clears the
// schedule; a non-empty cron is validated (400 bad_request naming "cron" on
// bad syntax).
func (s *server) handleUpdatePipeline(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	var req struct {
		YAML   *string `json:"yaml"`
		Engine *string `json:"engine"`
		Cron   *string `json:"cron"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	updated, err := s.updatePipeline(p, pl, req.YAML, req.Engine, req.Cron)
	if err != nil {
		if writePipelineRequestError(w, err) {
			return
		}
		log.Printf("update pipeline: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, pipelineDetailJSON(updated))
}

// handleListPipelines lists a project's pipelines, each with its newest run
// summary.
func (s *server) handleListPipelines(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pipelines, err := s.st.ListPipelines(p.ID)
	if err != nil {
		log.Printf("list pipelines: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(pipelines))
	for _, pl := range pipelines {
		runs, err := s.st.ListPipelineRuns(pl.ID)
		if err != nil {
			log.Printf("list runs for pipeline %s: %v", pl.ID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		var lastRun *store.PipelineRun
		if len(runs) > 0 {
			lastRun = &runs[0] // ListPipelineRuns is newest first
		}
		out = append(out, pipelineListJSON(pl, lastRun))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetPipeline returns one pipeline's detail, including its yaml.
func (s *server) handleGetPipeline(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pipelineDetailJSON(pl))
}

// handleDeletePipeline deletes a pipeline, refusing while any of its runs is
// still in progress (409 pipeline_busy) — mirrors sweeps' "stop before you
// can lose it" instinct, but pipelines have no auto-stop-on-delete: the
// operator must stop the run first.
func (s *server) handleDeletePipeline(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	runs, err := s.st.ListPipelineRuns(pl.ID)
	if err != nil {
		log.Printf("list runs for pipeline %s: %v", pl.ID, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	for _, run := range runs {
		if run.Status == "running" {
			writeError(w, http.StatusConflict, "pipeline_busy", "pipeline has a run in progress")
			return
		}
	}

	if err := s.st.DeletePipeline(pl.ID); err != nil {
		log.Printf("delete pipeline: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCreatePipelineRun manually triggers a pipeline run.
func (s *server) handleCreatePipelineRun(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	run, steps, err := s.startPipelineRun(r.Context(), pl, "manual")
	if err != nil {
		if writePipelineRequestError(w, err) {
			return
		}
		log.Printf("start pipeline run: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, pipelineRunDetailJSON(run, steps))
}

// handleListPipelineRuns lists a pipeline's run history (newest first,
// capped at 50 by the store).
func (s *server) handleListPipelineRuns(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	runs, err := s.st.ListPipelineRuns(pl.ID)
	if err != nil {
		log.Printf("list pipeline runs: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, pipelineRunBaseJSON(run))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetPipelineRun returns one run plus its steps in row (topo) order.
func (s *server) handleGetPipelineRun(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	run, ok := s.requirePipelineRun(w, pl, r.PathValue("id"))
	if !ok {
		return
	}
	steps, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		log.Printf("list run steps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, pipelineRunDetailJSON(run, steps))
}

// handleStopPipelineRun idempotently stops a run: a running run is torn down
// via stopPipelineRun and finishes "stopped"; an already-finished run is a
// 200 no-op that just reports current state (B2 stopSweep convention).
func (s *server) handleStopPipelineRun(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	run, ok := s.requirePipelineRun(w, pl, r.PathValue("id"))
	if !ok {
		return
	}

	if run.Status == "running" {
		if err := s.stopPipelineRun(r.Context(), run); err != nil {
			log.Printf("stop pipeline run %s: %v", run.ID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		got, err := s.st.GetPipelineRun(run.ID)
		if err != nil {
			log.Printf("get pipeline run %s: %v", run.ID, err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		run = got
	}

	steps, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		log.Printf("list run steps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, pipelineRunDetailJSON(run, steps))
}

// --- webhook trigger (C2 Task 3) ----------------------------------------

// pipelineWebhookPath returns the public (unauthenticated) hook URL path an
// external system posts to, to trigger pipeline id. Deviates from the spec's
// /v1/hooks/... in favor of the repo's existing hook-path convention
// (webhook.go's webhookPath for app deploy hooks lives at /hooks/apps/...).
func pipelineWebhookPath(id string) string {
	return "/hooks/pipelines/" + id
}

// generatePipelineWebhookSecret is handleGeneratePipelineWebhookSecret's
// shared core: generate a fresh 32-byte secret (hex-encoded, 64 chars — what
// the caller signs/compares with), seal it, and store it. Always
// regenerates: calling this on a pipeline that already has a secret rotates
// it, invalidating the old one — same convention as enableWebhook
// (webhook.go) for app deploy hooks. Returns the plaintext hex secret — the
// ONLY time it is ever available in plaintext; only the sealed form
// persists.
func (s *server) generatePipelineWebhookSecret(pl store.Pipeline) (string, error) {
	if s.sealer == nil {
		return "", errSealerUnavailable
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secretHex := hex.EncodeToString(raw)
	sealed, err := s.sealer.Seal([]byte(secretHex))
	if err != nil {
		return "", err
	}
	if err := s.st.SetPipelineWebhookSecret(pl.ID, sealed); err != nil {
		return "", err
	}
	return secretHex, nil
}

// handleGeneratePipelineWebhookSecret turns on (or rotates) a pipeline's
// trigger webhook. The secret is returned in this response ONLY — it is
// never recoverable from the store afterward (only the sealed bytes
// persist), mirroring handleWebhookEnable's contract for app deploy hooks.
func (s *server) handleGeneratePipelineWebhookSecret(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	secretHex, err := s.generatePipelineWebhookSecret(pl)
	if err != nil {
		if errors.Is(err, errSealerUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", errSealerUnavailable.Error())
			return
		}
		log.Printf("generate pipeline webhook secret: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url": pipelineWebhookPath(pl.ID), "secret": secretHex,
	})
}

// handleDeletePipelineWebhookSecret turns off a pipeline's trigger webhook:
// the stored secret is cleared, so any previously-issued secret stops
// verifying.
func (s *server) handleDeletePipelineWebhookSecret(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	pl, ok := s.requirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	if err := s.st.SetPipelineWebhookSecret(pl.ID, nil); err != nil {
		log.Printf("delete pipeline webhook secret: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writePipelineWebhookTriggerError maps startPipelineRun's sentinel errors to
// the webhook trigger's response: unlike the authenticated create-run
// endpoint (writePipelineRequestError, 400s), a signature-verified webhook
// call that can't fire is a 409 conflict — the request itself was fine, the
// pipeline just isn't in a runnable state (argo engine unavailable, or the
// stored yaml/app-refs have since drifted invalid).
func writePipelineWebhookTriggerError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errPipelineEngineUnavailable):
		writeError(w, http.StatusConflict, "engine_unavailable", err.Error())
	case errors.Is(err, errArgoNotInstalled):
		writeError(w, http.StatusConflict, "argo_not_installed", err.Error())
	case errors.Is(err, errPipelineAppMismatch):
		writeError(w, http.StatusConflict, "kind_mismatch", err.Error())
	case errors.Is(err, errBadPipelineRequest):
		writeError(w, http.StatusConflict, "bad_request", err.Error())
	default:
		return false
	}
	return true
}

// handlePipelineWebhookTrigger is the public (unauthenticated at the
// HTTP-auth layer) endpoint an external system posts to, to trigger a
// pipeline run. Auth is the HMAC/token check itself (verifyWebhook,
// webhook.go): every failure up to and including a bad signature answers
// with the identical 401 body — no existence oracle for a pipeline id,
// matching handleWebhookTrigger's convention for app deploy hooks. Unlike
// the cron trigger, a webhook fire is NOT Forbid-gated against a running
// previous run (spec: Forbid applies to cron only) — every valid call starts
// a fresh run. The request body itself is never parsed; it only needs to be
// read so verifyWebhook can hash it.
func (s *server) handlePipelineWebhookTrigger(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		webhookUnauthorized(w)
		return
	}

	pl, err := s.st.GetPipelineByID(r.PathValue("id"))
	if err != nil {
		webhookUnauthorized(w)
		return
	}
	if pl.WebhookSecret == nil || s.sealer == nil {
		webhookUnauthorized(w)
		return
	}
	plain, err := s.sealer.Open(pl.WebhookSecret)
	if err != nil {
		webhookUnauthorized(w)
		return
	}
	if !verifyWebhook(r, body, string(plain)) {
		webhookUnauthorized(w)
		return
	}

	if info := auditFrom(r.Context()); info != nil {
		info.Email = "webhook"
		info.Pattern = r.Pattern
	}

	// Authenticated from here on — failures are ordinary status codes.
	run, _, err := s.startPipelineRun(r.Context(), pl, "webhook")
	if err != nil {
		if writePipelineWebhookTriggerError(w, err) {
			return
		}
		log.Printf("pipeline webhook trigger: start run: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.ID})
}
