package server

import (
	"github.com/sutantodadang/luncur/internal/pipeline"
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
