package store

import (
	"path/filepath"
	"testing"
)

func sweepStore(t *testing.T) (*Store, App) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sweeps.db"))
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

func TestSweepLifecycle(t *testing.T) {
	st, a := sweepStore(t)

	sw := Sweep{
		AppID:     a.ID,
		Metric:    "val_loss",
		Direction: "min",
		MaxTrials: 3,
		Parallel:  2,
		EarlyStop: true,
		Nodes:     1,
		Framework: "",
		Seed:      42,
	}
	trialParams := []string{`{"lr":"0.1"}`, `{"lr":"0.01"}`, `{"lr":"0.001"}`}
	got, trials, err := st.CreateSweep(sw, trialParams)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == "" || got.Status != "running" || got.AppID != a.ID {
		t.Fatalf("created sweep: %+v", got)
	}
	if len(trials) != 3 {
		t.Fatalf("want 3 trials, got %d", len(trials))
	}
	for _, tr := range trials {
		if tr.State != "pending" || tr.SweepID != got.ID || tr.ID == "" {
			t.Fatalf("trial not pending: %+v", tr)
		}
		if tr.RunID.Valid || tr.MetricValue.Valid || tr.MetricStep.Valid {
			t.Fatalf("new trial must have no run/metric: %+v", tr)
		}
	}

	// launch one trial (run_id references a real job_runs row: foreign_keys
	// is enforced).
	run0, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTrialLaunched(trials[0].ID, run0.ID); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListTrials(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("ListTrials len = %d, want 3", len(list))
	}
	// oldest first: same order as trials returned by CreateSweep
	if list[0].ID != trials[0].ID || list[1].ID != trials[1].ID || list[2].ID != trials[2].ID {
		t.Fatalf("ListTrials order: %+v", list)
	}
	if list[0].State != "running" || !list[0].RunID.Valid || list[0].RunID.Int64 != run0.ID {
		t.Fatalf("launched trial: %+v", list[0])
	}

	// finish the launched trial as done with a metric value
	val := 0.42
	step := int64(100)
	if err := st.FinishTrial(trials[0].ID, "done", &val, &step); err != nil {
		t.Fatal(err)
	}
	got0, err := st.ListTrials(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got0[0].State != "done" || !got0[0].MetricValue.Valid || got0[0].MetricValue.Float64 != 0.42 ||
		!got0[0].MetricStep.Valid || got0[0].MetricStep.Int64 != 100 {
		t.Fatalf("finished trial: %+v", got0[0])
	}

	// launch and finish another as pruned, with nil value
	run1, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTrialLaunched(trials[1].ID, run1.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTrial(trials[1].ID, "pruned", nil, nil); err != nil {
		t.Fatal(err)
	}
	got1, err := st.ListTrials(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1[1].State != "pruned" || got1[1].MetricValue.Valid || got1[1].MetricStep.Valid {
		t.Fatalf("pruned trial: %+v", got1[1])
	}

	// FinishSweep done
	if err := st.FinishSweep(got.ID, "done"); err != nil {
		t.Fatal(err)
	}
	final, err := st.GetSweep(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "done" {
		t.Fatalf("final sweep status = %q, want done", final.Status)
	}

	active, err := st.ActiveSweeps()
	if err != nil {
		t.Fatal(err)
	}
	for _, as := range active {
		if as.ID == got.ID {
			t.Fatalf("finished sweep still active: %+v", as)
		}
	}
}

func TestSweepWarning(t *testing.T) {
	st, a := sweepStore(t)
	sw, _, err := st.CreateSweep(Sweep{
		AppID: a.ID, Metric: "acc", Direction: "max", MaxTrials: 1, Parallel: 1,
	}, []string{`{}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSweepWarning(sw.ID, "mlflow unreachable"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Warning != "mlflow unreachable" {
		t.Fatalf("warning = %q", got.Warning)
	}
}

func TestSweepValidation(t *testing.T) {
	st, a := sweepStore(t)

	base := Sweep{AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1}

	bad := base
	bad.Direction = "sideways"
	if _, _, err := st.CreateSweep(bad, []string{`{}`}); err == nil {
		t.Fatal("bad direction must be rejected")
	}

	bad2 := base
	bad2.MaxTrials = 0
	if _, _, err := st.CreateSweep(bad2, []string{`{}`}); err == nil {
		t.Fatal("MaxTrials 0 must be rejected")
	}

	bad3 := base
	bad3.Parallel = 0
	if _, _, err := st.CreateSweep(bad3, []string{`{}`}); err == nil {
		t.Fatal("Parallel 0 must be rejected")
	}

	if _, _, err := st.CreateSweep(base, nil); err == nil {
		t.Fatal("empty trialParams must be rejected")
	}
	if _, _, err := st.CreateSweep(base, []string{}); err == nil {
		t.Fatal("empty trialParams must be rejected")
	}
}

func TestFinishTrialValidation(t *testing.T) {
	st, a := sweepStore(t)
	sw, trials, err := st.CreateSweep(Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1,
	}, []string{`{}`})
	if err != nil {
		t.Fatal(err)
	}
	_ = sw
	// trial is pending, not running: FinishTrial must reject the transition.
	if err := st.FinishTrial(trials[0].ID, "done", nil, nil); err == nil {
		t.Fatal("finishing a pending trial must fail")
	} else if err.Error() != "trial not running" {
		t.Fatalf("err = %v, want %q", err, "trial not running")
	}
}

func TestFinishSweepValidation(t *testing.T) {
	st, a := sweepStore(t)
	sw, _, err := st.CreateSweep(Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1,
	}, []string{`{}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSweep(sw.ID, "running"); err == nil {
		t.Fatal("finish status must be done|stopped|failed")
	}
	if err := st.FinishSweep("nope", "done"); err != ErrNotFound {
		t.Fatalf("missing sweep: %v, want ErrNotFound", err)
	}
}

func TestListSweepsOrdering(t *testing.T) {
	st, a := sweepStore(t)
	var ids []string
	for i := 0; i < 3; i++ {
		sw, _, err := st.CreateSweep(Sweep{
			AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1,
		}, []string{`{}`})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sw.ID)
	}
	list, err := st.ListSweeps(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 sweeps, got %d", len(list))
	}
	// newest first
	if list[0].ID != ids[2] || list[1].ID != ids[1] || list[2].ID != ids[0] {
		t.Fatalf("ListSweeps order: %+v, want reverse of %v", list, ids)
	}
}

func TestMarkTrialLaunchedValidation(t *testing.T) {
	st, a := sweepStore(t)
	_, trials, err := st.CreateSweep(Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1,
	}, []string{`{}`})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}
	run2, err := st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	// already running: launching again must fail (not pending anymore).
	if err := st.MarkTrialLaunched(trials[0].ID, run2.ID); err == nil {
		t.Fatal("launching an already-running trial must fail")
	}
	if err := st.MarkTrialLaunched("nope", run.ID); err != ErrNotFound {
		t.Fatalf("missing trial: %v, want ErrNotFound", err)
	}
}
