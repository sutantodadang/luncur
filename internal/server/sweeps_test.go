package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
	"github.com/sutantodadang/luncur/internal/sweep"
)

// sweepTestServer builds a bare *server (no HTTP listener) for exercising
// the sweep loop's unexported methods directly. dyn/cs may both be nil —
// tests that never reach a kube call (over-budget short-circuits before
// startRun touches the cluster; mlflow-only harvest via sweepMLflowURLFn)
// don't need one.
func sweepTestServer(t *testing.T, dyn *dynamicfake.FakeDynamicClient) *server {
	t.Helper()
	st := newTestStore(t)
	var kc *kube.Client
	if dyn != nil {
		kc = kube.NewForTest(dyn, nil)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return newServer(Deps{Store: st, Kube: kc, Sealer: sealer, ExternalIP: "1.2.3.4"})
}

// sweepSeedApp creates a project + deployed kind=job app ready to run
// sweeps against, without going through the HTTP/build layers.
func sweepSeedApp(t *testing.T, st *store.Store) (store.Project, store.App) {
	t.Helper()
	p, err := st.CreateProject("ml")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(a.ID, "live", "trainer:1", 0); err != nil {
		t.Fatal(err)
	}
	return p, a
}

func doneTrial(id string, val float64, step int64) trialView {
	return trialView{Trial: store.SweepTrial{
		ID: id, State: "done",
		MetricValue: sql.NullFloat64{Float64: val, Valid: true},
		MetricStep:  sql.NullInt64{Int64: step, Valid: true},
	}}
}

func runningTrial(id string, live sweep.Obs) trialView {
	return trialView{Trial: store.SweepTrial{ID: id, State: "running"}, Live: live}
}

func pendingTrial(id string) trialView {
	return trialView{Trial: store.SweepTrial{ID: id, State: "pending"}}
}

// --- decideSweep pure-core matrix -----------------------------------------

func TestDecideSweepLaunchRespectsParallel(t *testing.T) {
	sw := store.Sweep{Parallel: 2, Direction: "min"}
	trials := []trialView{
		runningTrial("r1", sweep.Obs{}),
		pendingTrial("p1"),
		pendingTrial("p2"),
		pendingTrial("p3"),
	}
	actions := decideSweep(sw, trials)
	if len(actions.Launch) != 1 || actions.Launch[0] != "p1" {
		t.Fatalf("launch = %v, want [p1] (1 running of parallel=2, oldest pending first)", actions.Launch)
	}
}

func TestDecideSweepNoPruneBeforeThreeDone(t *testing.T) {
	sw := store.Sweep{Parallel: 1, Direction: "min", EarlyStop: true}
	trials := []trialView{
		doneTrial("d1", 1.0, 10),
		doneTrial("d2", 2.0, 10),
		runningTrial("r1", sweep.Obs{Value: 100, Step: 10, Found: true}),
	}
	actions := decideSweep(sw, trials)
	if len(actions.Prune) != 0 {
		t.Fatalf("prune = %v, want none (only 2 done trials)", actions.Prune)
	}
}

func TestDecideSweepPrunesWorseThanMedianDirectionMin(t *testing.T) {
	sw := store.Sweep{Parallel: 1, Direction: "min", EarlyStop: true}
	trials := []trialView{
		doneTrial("d1", 1.0, 10),
		doneTrial("d2", 2.0, 10),
		doneTrial("d3", 3.0, 10),
		// median = 2.0; 5.0 > median -> worse for "min" -> prune.
		runningTrial("r1", sweep.Obs{Value: 5.0, Step: 10, Found: true}),
	}
	actions := decideSweep(sw, trials)
	if len(actions.Prune) != 1 || actions.Prune[0] != "r1" {
		t.Fatalf("prune = %v, want [r1]", actions.Prune)
	}
}

func TestDecideSweepPrunesWorseThanMedianDirectionMax(t *testing.T) {
	sw := store.Sweep{Parallel: 1, Direction: "max", EarlyStop: true}
	trials := []trialView{
		doneTrial("d1", 1.0, 10),
		doneTrial("d2", 2.0, 10),
		doneTrial("d3", 3.0, 10),
		// median = 2.0; 0.5 < median -> worse for "max" -> prune.
		runningTrial("r1", sweep.Obs{Value: 0.5, Step: 10, Found: true}),
	}
	actions := decideSweep(sw, trials)
	if len(actions.Prune) != 1 || actions.Prune[0] != "r1" {
		t.Fatalf("prune = %v, want [r1]", actions.Prune)
	}
}

func TestDecideSweepNoPruneWhenLiveNotFound(t *testing.T) {
	sw := store.Sweep{Parallel: 1, Direction: "min", EarlyStop: true}
	trials := []trialView{
		doneTrial("d1", 1.0, 10),
		doneTrial("d2", 2.0, 10),
		doneTrial("d3", 3.0, 10),
		runningTrial("r1", sweep.Obs{Value: 100, Step: 10, Found: false}),
	}
	actions := decideSweep(sw, trials)
	if len(actions.Prune) != 0 {
		t.Fatalf("prune = %v, want none (Live.Found false)", actions.Prune)
	}
}

func TestDecideSweepNoPruneBelowComparableStep(t *testing.T) {
	sw := store.Sweep{Parallel: 1, Direction: "min", EarlyStop: true}
	trials := []trialView{
		// minDoneStep = 10.
		doneTrial("d1", 1.0, 10),
		doneTrial("d2", 2.0, 20),
		doneTrial("d3", 3.0, 30),
		// Live.Step (5) < minDoneStep (10) -> not comparable, no prune even
		// though 100 is worse than the median (2.0) for direction min.
		runningTrial("r1", sweep.Obs{Value: 100, Step: 5, Found: true}),
	}
	actions := decideSweep(sw, trials)
	if len(actions.Prune) != 0 {
		t.Fatalf("prune = %v, want none (live step below smallest done step)", actions.Prune)
	}
}

func TestDecideSweepFinishOnlyWhenDrained(t *testing.T) {
	sw := store.Sweep{Parallel: 1, Direction: "min"}

	drained := []trialView{doneTrial("d1", 1.0, 10)}
	if !decideSweep(sw, drained).Finish {
		t.Fatalf("finish = false, want true (no pending/running trials)")
	}

	withPending := []trialView{doneTrial("d1", 1.0, 10), pendingTrial("p1")}
	if decideSweep(sw, withPending).Finish {
		t.Fatalf("finish = true, want false (a pending trial remains)")
	}

	withRunning := []trialView{doneTrial("d1", 1.0, 10), runningTrial("r1", sweep.Obs{})}
	if decideSweep(sw, withRunning).Finish {
		t.Fatalf("finish = true, want false (a running trial remains)")
	}
}

// --- loop-level tests (store + fakes) --------------------------------------

func TestSweepTickHarvestsFinishedRun(t *testing.T) {
	var hits int
	mlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"runs":[{"data":{"metrics":[{"key":"val_loss","value":0.42,"step":10}]}}]}`))
	}))
	t.Cleanup(mlSrv.Close)

	s := sweepTestServer(t, nil)
	s.sweepMLflowURLFn = func(store.App, string) string { return mlSrv.URL }
	_, a := sweepSeedApp(t, s.st)

	sw, trials, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1, Nodes: 1,
	}, []string{`{"lr":"0.1"}`})
	if err != nil {
		t.Fatal(err)
	}

	run, err := s.st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.FinishJobRun(run.ID, "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}

	s.sweepTick(context.Background())

	got, err := s.st.ListTrials(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "done" {
		t.Fatalf("trials = %+v, want 1 trial state=done", got)
	}
	if !got[0].MetricValue.Valid || got[0].MetricValue.Float64 != 0.42 {
		t.Fatalf("metric value = %+v, want 0.42", got[0].MetricValue)
	}
	if !got[0].MetricStep.Valid || got[0].MetricStep.Int64 != 10 {
		t.Fatalf("metric step = %+v, want 10", got[0].MetricStep)
	}
	if hits != 1 {
		t.Fatalf("mlflow hits = %d, want 1", hits)
	}

	gotSweep, err := s.st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSweep.Status != "done" {
		t.Fatalf("sweep status = %q, want done (drained same tick)", gotSweep.Status)
	}
}

func TestSweepTickOverBudgetLeavesPending(t *testing.T) {
	// No kube fake: the over-budget check happens purely in the store, so
	// startRun must never reach s.kube (nil here) for this trial.
	s := sweepTestServer(t, nil)
	p, a := sweepSeedApp(t, s.st)
	if err := s.st.SetProjectGPUQuota(p.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetGPU(a.ID, 1); err != nil {
		t.Fatal(err)
	}

	// nodes=3 vs the app's default nodes=1 needs 1*(3-1)=2 more GPUs; the
	// app's own footprint (1) is already counted against the quota (2), so
	// there's only 1 free -> over budget.
	sw, _, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1, Nodes: 3,
	}, []string{`{"lr":"0.1"}`})
	if err != nil {
		t.Fatal(err)
	}

	s.sweepTick(context.Background())

	got, err := s.st.ListTrials(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "pending" {
		t.Fatalf("trials = %+v, want 1 trial state=pending (over budget, retried next tick)", got)
	}
}

func TestSweepTickMlflowDegradeSetsWarningOnce(t *testing.T) {
	var hits int
	mlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(mlSrv.Close)

	s := sweepTestServer(t, nil)
	s.sweepMLflowURLFn = func(store.App, string) string { return mlSrv.URL }
	_, a := sweepSeedApp(t, s.st)

	sw, trials, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1, Nodes: 1, EarlyStop: true,
	}, []string{`{"lr":"0.1"}`})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.st.CreateJobRun(a.ID, 1, "") // stays "running"
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}

	s.sweepTick(context.Background())

	gotSweep, err := s.st.GetSweep(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSweep.Warning == "" {
		t.Fatalf("sweep warning not set after mlflow failure")
	}
	if !s.sweepMLflowDown[sw.ID] {
		t.Fatalf("sweepMLflowDown[%s] = false, want true", sw.ID)
	}
	if hits != 1 {
		t.Fatalf("mlflow hits after first tick = %d, want 1", hits)
	}

	// Second tick must not retry mlflow (degraded for the sweep's lifetime).
	s.sweepTick(context.Background())
	if hits != 1 {
		t.Fatalf("mlflow hits after second tick = %d, want still 1 (degraded)", hits)
	}
}

func TestSweepReconcileMarksOrphansFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme) // no Job objects seeded -> JobExists is false
	s := sweepTestServer(t, dyn)
	_, a := sweepSeedApp(t, s.st)

	sw, trials, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1, Nodes: 1,
	}, []string{`{"lr":"0.1"}`})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.st.CreateJobRun(a.ID, 1, "") // status stays "running"
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}

	s.sweepReconcile(context.Background())

	gotRun, err := s.st.GetJobRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "failed" {
		t.Fatalf("run status = %q, want failed (orphaned, job gone)", gotRun.Status)
	}
	gotTrials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTrials) != 1 || gotTrials[0].State != "failed" {
		t.Fatalf("trials = %+v, want 1 trial state=failed", gotTrials)
	}
}

func TestSweepReconcileLeavesRunningJobAlone(t *testing.T) {
	// CreateProject("ml") always derives namespace "luncur-ml", and this
	// is a fresh store's first job run, so jobRunName("train", 1) ==
	// "train-run-1" — both computed independently of s here so the Job
	// object can be seeded before the server/store exist.
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "train-run-1", "namespace": "luncur-ml"},
	}}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, job)
	s := sweepTestServer(t, dyn)
	_, a := sweepSeedApp(t, s.st)

	sw, trials, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1, Nodes: 1,
	}, []string{`{"lr":"0.1"}`})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}

	s.sweepReconcile(context.Background())

	gotRun, err := s.st.GetJobRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "running" {
		t.Fatalf("run status = %q, want running (job exists, leave alone)", gotRun.Status)
	}
	gotTrials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTrials) != 1 || gotTrials[0].State != "running" {
		t.Fatalf("trials = %+v, want 1 trial state=running", gotTrials)
	}
}
