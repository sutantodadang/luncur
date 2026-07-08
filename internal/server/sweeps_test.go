package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

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

// --- HTTP endpoints (Task 5) ------------------------------------------

// sweepAPIServer builds a full HTTP test server with both the dynamic and
// typed-clientset halves of the fake kube layer wired (DeleteJob, used by
// the stop endpoint, goes through the typed clientset — see
// kube.Client.DeleteJob), recording every action verb+resource.
func sweepAPIServer(t *testing.T) (*httptestServer, *store.Store, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return false, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(dyn, cs), Sealer: sealer, ExternalIP: "1.2.3.4"})
	return srv, st, &actions
}

func TestCreateSweepHappyPath(t *testing.T) {
	srv, st, _ := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	body := `{"params_yaml":"a: [\"1\",\"2\"]","metric":"val_loss","direction":"min","max_trials":10,"parallel":1}`
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps", admin, body)
	respBody := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create sweep: want 202, got %d (%s)", resp.StatusCode, respBody)
	}
	var got map[string]any
	if err := json.Unmarshal(respBody, &got); err != nil {
		t.Fatal(err)
	}
	trials, _ := got["trials"].([]any)
	if len(trials) != 2 {
		t.Fatalf("trials = %+v, want 2 (grid of a=[1,2])", got["trials"])
	}
	for _, tr := range trials {
		m := tr.(map[string]any)
		if m["state"] != "pending" {
			t.Fatalf("trial state = %v, want pending", m["state"])
		}
	}
	if got["status"] != "running" {
		t.Fatalf("sweep status = %v, want running", got["status"])
	}
}

func TestCreateSweepOnWebAppKindMismatch(t *testing.T) {
	srv, st, _ := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/api/sweeps", admin, `{}`)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, mustReadBody(t, resp)) != "kind_mismatch" {
		t.Fatalf("sweep on web app: want 400 kind_mismatch, got %d", resp.StatusCode)
	}
}

func TestCreateSweepBadYAML(t *testing.T) {
	srv, st, _ := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	body := `{"params_yaml":"lr: 0.1","metric":"val_loss","direction":"min","max_trials":10,"parallel":1}`
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps", admin, body)
	respBody := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, respBody) != "bad_request" {
		t.Fatalf("bad params.yaml: want 400 bad_request, got %d (%s)", resp.StatusCode, respBody)
	}
	if !bytesContains(respBody, `lr`) {
		t.Fatalf("bad params.yaml error should name the offending key, got %s", respBody)
	}
}

func TestCreateSweepNotDeployed(t *testing.T) {
	srv, st, _ := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	body := `{"params_yaml":"a: [\"1\"]","metric":"val_loss","direction":"min","max_trials":10,"parallel":1}`
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps", admin, body)
	if resp.StatusCode != http.StatusConflict || errCode(t, mustReadBody(t, resp)) != "not_deployed" {
		t.Fatalf("undeployed sweep: want 409 not_deployed, got %d", resp.StatusCode)
	}
}

func TestGetSweepReturnsBestTrial(t *testing.T) {
	srv, st, _ := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	body := `{"params_yaml":"a: [\"1\",\"2\",\"3\"]","metric":"val_loss","direction":"min","max_trials":10,"parallel":1}`
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps", admin, body)
	var created map[string]any
	json.Unmarshal(mustReadBody(t, resp), &created)
	sweepID := created["id"].(string)
	trials := created["trials"].([]any)

	// Finish two trials done with distinct values; direction min -> the
	// lower value must win as best. FinishTrial only accepts running ->
	// done, so launch each trial (a fake run row) first.
	appID := appID(t, st, "ml", "train")
	vals := []float64{0.5, 0.2}
	for i, v := range vals {
		trID := trials[i].(map[string]any)["id"].(string)
		run, err := st.CreateJobRun(appID, 1, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkTrialLaunched(trID, run.ID); err != nil {
			t.Fatal(err)
		}
		val := v
		if err := st.FinishTrial(trID, "done", &val, nil); err != nil {
			t.Fatal(err)
		}
	}

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/sweeps/"+sweepID, admin, "")
	var got map[string]any
	respBody := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get sweep: want 200, got %d (%s)", resp.StatusCode, respBody)
	}
	json.Unmarshal(respBody, &got)
	bestTrialID := trials[1].(map[string]any)["id"].(string) // value 0.2, the lower of the two
	if got["best_trial_id"] != bestTrialID {
		t.Fatalf("best_trial_id = %v, want %v (lowest value for direction min)", got["best_trial_id"], bestTrialID)
	}
	if got["best_value"] != 0.2 {
		t.Fatalf("best_value = %v, want 0.2", got["best_value"])
	}
}

func TestStopSweepKillsRunningTrialsAndIsIdempotent(t *testing.T) {
	srv, st, actions := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`).Body.Close()

	body := `{"params_yaml":"a: [\"1\"]","metric":"val_loss","direction":"min","max_trials":10,"parallel":1}`
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps", admin, body)
	var created map[string]any
	json.Unmarshal(mustReadBody(t, resp), &created)
	sweepID := created["id"].(string)
	trialID := created["trials"].([]any)[0].(map[string]any)["id"].(string)

	appID := appID(t, st, "ml", "train")
	run, err := st.CreateJobRun(appID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTrialLaunched(trialID, run.ID); err != nil {
		t.Fatal(err)
	}

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps/"+sweepID+"/stop", admin, "")
	respBody := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop sweep: want 200, got %d (%s)", resp.StatusCode, respBody)
	}
	var got map[string]any
	json.Unmarshal(respBody, &got)
	if got["status"] != "stopped" {
		t.Fatalf("sweep status = %v, want stopped", got["status"])
	}

	gotTrial, err := st.ListTrials(sweepID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTrial) != 1 || gotTrial[0].State != "pruned" {
		t.Fatalf("trials = %+v, want 1 trial state=pruned", gotTrial)
	}
	gotRun, err := st.GetJobRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "failed" {
		t.Fatalf("run status = %q, want failed (killed by stop)", gotRun.Status)
	}

	var deletes int
	for _, a := range *actions {
		if a == "delete jobs" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("delete job actions = %d, want 1", deletes)
	}

	// Second stop is an idempotent no-op: no further deletes, still 200.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/sweeps/"+sweepID+"/stop", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second stop: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	deletes = 0
	for _, a := range *actions {
		if a == "delete jobs" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("delete job actions after second stop = %d, want still 1 (idempotent)", deletes)
	}
}

func TestSweepEndpointsNonMemberForbidden(t *testing.T) {
	srv, st, _ := sweepAPIServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	outsider := seedUserToken(t, st, "outsider@b.co", "member")
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/sweeps", outsider, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member list sweeps: want 403, got %d", resp.StatusCode)
	}
}

func bytesContains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}
