package server

import (
	"testing"

	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/store"
)

// stepView builds a pipeStepView for the decidePipelineRun matrix: a row in
// the given state plus its compiled spec step (kind + needs). Run is nil
// (attach separately for the app-retry cases); Row.ID == name for
// readability in assertions.
func stepView(name, kind, state string, needs []string) pipeStepView {
	return pipeStepView{
		Row:  store.PipelineRunStep{ID: name, Name: name, State: state},
		Spec: pipeline.Step{Name: name, Kind: kind, Needs: needs},
	}
}

// specOf reconstructs the pipeline.Spec a set of views were compiled from —
// decidePipelineRun needs it (via spec.Downstream) independently of the
// views slice, exactly like the real caller passes the run's stored
// spec_json alongside its step rows.
func specOf(views []pipeStepView) pipeline.Spec {
	steps := make([]pipeline.Step, 0, len(views))
	for _, v := range views {
		steps = append(steps, v.Spec)
	}
	return pipeline.Spec{Steps: steps}
}

func launchIDs(t *testing.T, views []pipeStepView) pipeActions {
	t.Helper()
	return decidePipelineRun(specOf(views), views)
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// --- decidePipelineRun pure-core matrix ------------------------------------

func TestDecidePipelineRunParallelRootsBothLaunch(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "pending", nil),
		stepView("b", "app", "pending", nil),
	}
	actions := launchIDs(t, views)
	if len(actions.Launch) != 2 || !containsID(actions.Launch, "a") || !containsID(actions.Launch, "b") {
		t.Fatalf("Launch = %v, want [a b]", actions.Launch)
	}
	if len(actions.Skip) != 0 {
		t.Fatalf("Skip = %v, want none", actions.Skip)
	}
	if actions.Finish != "" {
		t.Fatalf("Finish = %q, want \"\"", actions.Finish)
	}
}

func TestDecidePipelineRunDiamondJoinWaitsForBoth(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "done", nil),
		stepView("b", "app", "pending", []string{"a"}),
		stepView("c", "app", "pending", []string{"a"}),
		stepView("d", "app", "pending", []string{"b", "c"}),
	}
	actions := launchIDs(t, views)
	if len(actions.Launch) != 2 || !containsID(actions.Launch, "b") || !containsID(actions.Launch, "c") {
		t.Fatalf("Launch = %v, want [b c] (d must wait for both)", actions.Launch)
	}
	if containsID(actions.Launch, "d") {
		t.Fatalf("Launch = %v, d must not launch until b and c are both done", actions.Launch)
	}
}

func TestDecidePipelineRunDiamondJoinLaunchesOnceBothDone(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "done", nil),
		stepView("b", "app", "done", []string{"a"}),
		stepView("c", "app", "done", []string{"a"}),
		stepView("d", "app", "pending", []string{"b", "c"}),
	}
	actions := launchIDs(t, views)
	if len(actions.Launch) != 1 || actions.Launch[0] != "d" {
		t.Fatalf("Launch = %v, want [d]", actions.Launch)
	}
}

func TestDecidePipelineRunFailedStepSkipsDownstreamKeepsSibling(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "failed", nil),
		stepView("b", "app", "pending", []string{"a"}), // downstream of failed a
		stepView("s", "app", "pending", nil),           // unrelated sibling branch
	}
	actions := launchIDs(t, views)
	if len(actions.Skip) != 1 || actions.Skip[0] != "b" {
		t.Fatalf("Skip = %v, want [b]", actions.Skip)
	}
	if len(actions.Launch) != 1 || actions.Launch[0] != "s" {
		t.Fatalf("Launch = %v, want [s] (sibling branch keeps launching)", actions.Launch)
	}
}

func TestDecidePipelineRunRunningAppRetryUnderBudgetLaunches(t *testing.T) {
	views := []pipeStepView{
		{
			Row:  store.PipelineRunStep{ID: "r", Name: "r", State: "running", Attempt: 1},
			Spec: pipeline.Step{Name: "r", Kind: "app", Retries: 2},
			Run:  &store.JobRun{Status: "failed"},
		},
	}
	actions := launchIDs(t, views)
	if len(actions.Launch) != 1 || actions.Launch[0] != "r" {
		t.Fatalf("Launch = %v, want [r] (attempt 1 < retries 2, must retry)", actions.Launch)
	}
}

func TestDecidePipelineRunRunningAppRetryExhaustedDoesNotLaunch(t *testing.T) {
	views := []pipeStepView{
		{
			Row:  store.PipelineRunStep{ID: "r", Name: "r", State: "running", Attempt: 2},
			Spec: pipeline.Step{Name: "r", Kind: "app", Retries: 2},
			Run:  &store.JobRun{Status: "failed"},
		},
	}
	actions := launchIDs(t, views)
	if len(actions.Launch) != 0 {
		t.Fatalf("Launch = %v, want none (attempt == retries, engine fails it instead)", actions.Launch)
	}
	if actions.Finish != "" {
		t.Fatalf("Finish = %q, want \"\" (row still running from decide's point of view)", actions.Finish)
	}
}

func TestDecidePipelineRunAllDoneFinishesDone(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "done", nil),
		stepView("b", "app", "done", []string{"a"}),
	}
	actions := launchIDs(t, views)
	if actions.Finish != "done" {
		t.Fatalf("Finish = %q, want done", actions.Finish)
	}
}

func TestDecidePipelineRunFailedAndSkippedDrainedFinishesFailed(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "failed", nil),
		stepView("b", "app", "skipped", []string{"a"}),
	}
	actions := launchIDs(t, views)
	if actions.Finish != "failed" {
		t.Fatalf("Finish = %q, want failed", actions.Finish)
	}
}

// Same-tick skip: b is still "pending" in this tick's view (the engine
// hasn't applied Skip yet), so Finish must stay "" even though the run has
// nothing left to launch — Finish only fires once a later tick observes b
// already skipped (see TestDecidePipelineRunFailedAndSkippedDrainedFinishesFailed).
func TestDecidePipelineRunFinishEmptyWhileSkipPending(t *testing.T) {
	views := []pipeStepView{
		stepView("a", "app", "failed", nil),
		stepView("b", "app", "pending", []string{"a"}),
	}
	actions := launchIDs(t, views)
	if len(actions.Skip) != 1 || actions.Skip[0] != "b" {
		t.Fatalf("Skip = %v, want [b]", actions.Skip)
	}
	if actions.Finish != "" {
		t.Fatalf("Finish = %q, want \"\" (b hasn't been marked skipped yet this tick)", actions.Finish)
	}
}
