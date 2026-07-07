package store

import (
	"path/filepath"
	"testing"
)

func jobRunStore(t *testing.T) (*Store, App) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := st.CreateProject("mlp")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	return st, a
}

func TestJobKindValidation(t *testing.T) {
	st, _ := jobRunStore(t)
	p, _ := st.CreateProject("mlp2")
	if _, err := st.CreateApp(p.ID, "bad", 8080, "job", ""); err == nil {
		t.Fatal("job app with a port must be rejected")
	}
	if _, err := st.CreateApp(p.ID, "bad2", 0, "job", "0 * * * *"); err == nil {
		t.Fatal("job app with a schedule must be rejected")
	}
}

func TestJobRunLifecycle(t *testing.T) {
	st, a := jobRunStore(t)
	run, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" || run.AppID != a.ID || run.StartedAt == "" {
		t.Fatalf("new run: %+v", run)
	}
	if run.ExitCode.Valid || run.FinishedAt.Valid {
		t.Fatalf("new run must not be finished: %+v", run)
	}

	code := int64(3)
	if err := st.FinishJobRun(run.ID, "failed", &code); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetJobRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || !got.ExitCode.Valid || got.ExitCode.Int64 != 3 || !got.FinishedAt.Valid {
		t.Fatalf("finished run: %+v", got)
	}

	// Second run with unknown exit code.
	run2, _ := st.CreateJobRun(a.ID, 1, "")
	if err := st.FinishJobRun(run2.ID, "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.GetJobRun(run2.ID)
	if got2.Status != "succeeded" || got2.ExitCode.Valid {
		t.Fatalf("run2: %+v", got2)
	}

	list, err := st.ListJobRuns(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != run2.ID || list[1].ID != run.ID {
		t.Fatalf("list order: %+v", list)
	}
}

func TestFinishJobRunValidation(t *testing.T) {
	st, a := jobRunStore(t)
	run, _ := st.CreateJobRun(a.ID, 1, "")
	if err := st.FinishJobRun(run.ID, "running", nil); err == nil {
		t.Fatal("finish with status running must be rejected")
	}
	if err := st.FinishJobRun(99999, "failed", nil); err != ErrNotFound {
		t.Fatalf("missing run: %v, want ErrNotFound", err)
	}
}

func TestJobRunNodesFramework(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("trainp")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateJobRun(a.ID, 4, "torch")
	if err != nil {
		t.Fatal(err)
	}
	if r.Nodes != 4 || r.Framework != "torch" {
		t.Fatalf("got nodes=%d framework=%q, want 4 torch", r.Nodes, r.Framework)
	}
	got, err := s.GetJobRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes != 4 || got.Framework != "torch" {
		t.Fatalf("read back: nodes=%d framework=%q", got.Nodes, got.Framework)
	}
}
