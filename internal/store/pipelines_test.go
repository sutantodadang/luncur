package store

import (
	"path/filepath"
	"testing"
)

func pipelineStore(t *testing.T) (*Store, Project) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "pipelines.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := st.CreateProject("mlp")
	if err != nil {
		t.Fatal(err)
	}
	return st, p
}

func TestPipelineLifecycle(t *testing.T) {
	st, p := pipelineStore(t)

	pl, err := st.CreatePipeline(Pipeline{
		ProjectID: p.ID,
		Name:      "train-pipe",
		YAML:      "steps:\n  a:\n    app: train\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pl.ID == "" || pl.Name != "train-pipe" || pl.ProjectID != p.ID {
		t.Fatalf("created pipeline: %+v", pl)
	}

	// duplicate name in the same project must be rejected.
	if _, err := st.CreatePipeline(Pipeline{ProjectID: p.ID, Name: "train-pipe", YAML: "steps:\n  a:\n    app: train\n"}); err == nil {
		t.Fatal("duplicate pipeline name must be rejected")
	}

	got, err := st.GetPipeline(p.ID, "train-pipe")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != pl.ID {
		t.Fatalf("GetPipeline: %+v", got)
	}

	list, err := st.ListPipelines(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != pl.ID {
		t.Fatalf("ListPipelines: %+v", list)
	}

	if err := st.UpdatePipeline(pl.ID, "steps:\n  a:\n    app: train2\n", "0 3 * * *", "argo"); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetPipeline(p.ID, "train-pipe")
	if err != nil {
		t.Fatal(err)
	}
	if updated.YAML != "steps:\n  a:\n    app: train2\n" || updated.Cron != "0 3 * * *" || updated.Engine != "argo" {
		t.Fatalf("updated pipeline: %+v", updated)
	}

	// create run with 3 steps, in insertion order.
	stepNamesKinds := [][2]string{{"prepare", "app"}, {"train", "app"}, {"evaluate", "image"}}
	run, steps, err := st.CreatePipelineRun(PipelineRun{
		PipelineID: pl.ID,
		Status:     "running",
		SpecJSON:   `{"steps":[]}`,
		Trigger:    "manual",
	}, stepNamesKinds)
	if err != nil {
		t.Fatal(err)
	}
	if run.ID == "" || run.Status != "running" || run.PipelineID != pl.ID {
		t.Fatalf("created run: %+v", run)
	}
	if len(steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(steps))
	}
	for i, want := range stepNamesKinds {
		if steps[i].Name != want[0] || steps[i].Kind != want[1] || steps[i].State != "pending" || steps[i].RunID != run.ID {
			t.Fatalf("step %d: %+v", i, steps[i])
		}
	}

	// ActivePipelineRuns must include this run.
	active, err := st.ActivePipelineRuns()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range active {
		if r.ID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ActivePipelineRuns missing run: %+v", active)
	}

	// MarkStepRunning without a jobRunID first (kind=image step launch).
	if err := st.MarkStepRunning(steps[0].ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != "running" || rows[0].Attempt != 0 {
		t.Fatalf("step0 after MarkStepRunning: %+v", rows[0])
	}

	// retry bump attempt (running -> running again with new attempt/jobRunID).
	// job_run_id has an FK to job_runs, so use a real row.
	a, err := st.CreateApp(p.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	jr, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkStepRunning(steps[0].ID, &jr.ID, 1); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != "running" || rows[0].Attempt != 1 || !rows[0].JobRunID.Valid || rows[0].JobRunID.Int64 != jr.ID {
		t.Fatalf("step0 after retry: %+v", rows[0])
	}

	// FinishStep done.
	if err := st.FinishStep(steps[0].ID, "done", "exit 0"); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != "done" || rows[0].Detail != "exit 0" {
		t.Fatalf("step0 after FinishStep done: %+v", rows[0])
	}

	// FinishStep skipped from pending (fail-fast downstream skip).
	if err := st.FinishStep(steps[2].ID, "skipped", "upstream failed"); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[2].State != "skipped" || rows[2].Detail != "upstream failed" {
		t.Fatalf("step2 after FinishStep skipped: %+v", rows[2])
	}

	// finish the remaining step so we can finish the run.
	if err := st.MarkStepRunning(steps[1].ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(steps[1].ID, "done", ""); err != nil {
		t.Fatal(err)
	}

	if err := st.FinishPipelineRun(run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	finished, err := st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "done" || !finished.FinishedAt.Valid {
		t.Fatalf("finished run: %+v", finished)
	}

	active, err = st.ActivePipelineRuns()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range active {
		if r.ID == run.ID {
			t.Fatalf("finished run still active: %+v", r)
		}
	}

	if err := st.SetPipelineRunWarning(run.ID, "mlflow degraded"); err != nil {
		t.Fatal(err)
	}
	withWarning, err := st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withWarning.Warning != "mlflow degraded" {
		t.Fatalf("warning: %+v", withWarning)
	}
}

func TestPipelineRunValidation(t *testing.T) {
	st, p := pipelineStore(t)
	pl, err := st.CreatePipeline(Pipeline{ProjectID: p.ID, Name: "pl1", YAML: "steps:\n  a:\n    app: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	run, steps, err := st.CreatePipelineRun(PipelineRun{
		PipelineID: pl.ID, Status: "running", SpecJSON: `{}`, Trigger: "manual",
	}, [][2]string{{"a", "app"}})
	if err != nil {
		t.Fatal(err)
	}

	// bad run status on FinishPipelineRun.
	if err := st.FinishPipelineRun(run.ID, "running"); err == nil {
		t.Fatal("FinishPipelineRun to running must be rejected")
	}

	// FinishStep to "pending" is not a legal target state.
	if err := st.FinishStep(steps[0].ID, "pending", ""); err == nil {
		t.Fatal("FinishStep to pending must be rejected")
	}

	// FinishStep "done" from pending must be rejected (only failed/skipped
	// are legal straight out of pending).
	if err := st.FinishStep(steps[0].ID, "done", ""); err == nil {
		t.Fatal("FinishStep done from pending must be rejected")
	}

	// but pending -> failed and pending -> skipped ARE legal.
	run2, steps2, err := st.CreatePipelineRun(PipelineRun{
		PipelineID: pl.ID, Status: "running", SpecJSON: `{}`, Trigger: "manual",
	}, [][2]string{{"a", "app"}, {"b", "app"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishStep(steps2[0].ID, "failed", "unlaunchable"); err != nil {
		t.Fatalf("pending -> failed must be legal: %v", err)
	}
	if err := st.FinishStep(steps2[1].ID, "skipped", "upstream failed"); err != nil {
		t.Fatalf("pending -> skipped must be legal: %v", err)
	}
	_ = run2
}

func TestListPipelineRunsOrderingAndCap(t *testing.T) {
	st, p := pipelineStore(t)
	pl, err := st.CreatePipeline(Pipeline{ProjectID: p.ID, Name: "pl1", YAML: "steps:\n  a:\n    app: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		run, _, err := st.CreatePipelineRun(PipelineRun{
			PipelineID: pl.ID, Status: "running", SpecJSON: `{}`, Trigger: "manual",
		}, [][2]string{{"a", "app"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	list, err := st.ListPipelineRuns(pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 runs, got %d", len(list))
	}
	// newest first
	if list[0].ID != ids[2] || list[1].ID != ids[1] || list[2].ID != ids[0] {
		t.Fatalf("ListPipelineRuns order: %+v, want reverse of %v", list, ids)
	}
}

// TestCronPipelinesFiltersEmptyCron covers CronPipelines: only pipelines with
// a non-empty cron come back, across projects.
func TestCronPipelinesFiltersEmptyCron(t *testing.T) {
	st, p := pipelineStore(t)
	noCron, err := st.CreatePipeline(Pipeline{ProjectID: p.ID, Name: "no-cron", YAML: "steps:\n  a:\n    app: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	withCron, err := st.CreatePipeline(Pipeline{ProjectID: p.ID, Name: "with-cron", YAML: "steps:\n  a:\n    app: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePipeline(withCron.ID, withCron.YAML, "0 3 * * *", withCron.Engine); err != nil {
		t.Fatal(err)
	}

	got, err := st.CronPipelines()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != withCron.ID || got[0].Cron != "0 3 * * *" {
		t.Fatalf("CronPipelines = %+v, want only %s with cron set", got, withCron.ID)
	}
	for _, pl := range got {
		if pl.ID == noCron.ID {
			t.Fatalf("CronPipelines must not include pipeline with empty cron: %+v", pl)
		}
	}
}

func TestDeletePipelineCascades(t *testing.T) {
	st, p := pipelineStore(t)
	pl, err := st.CreatePipeline(Pipeline{ProjectID: p.ID, Name: "pl1", YAML: "steps:\n  a:\n    app: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	run, steps, err := st.CreatePipelineRun(PipelineRun{
		PipelineID: pl.ID, Status: "running", SpecJSON: `{}`, Trigger: "manual",
	}, [][2]string{{"a", "app"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.DeletePipeline(pl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPipeline(p.ID, "pl1"); err != ErrNotFound {
		t.Fatalf("deleted pipeline still found: %v", err)
	}
	if _, err := st.GetPipelineRun(run.ID); err != ErrNotFound {
		t.Fatalf("run must cascade-delete: %v", err)
	}
	rows, err := st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("run steps must cascade-delete: %+v", rows)
	}
	_ = steps

	if err := st.DeletePipeline("nope"); err != ErrNotFound {
		t.Fatalf("delete missing pipeline: %v, want ErrNotFound", err)
	}
}
