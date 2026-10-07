package server

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// A trial's Job lives in the app's environment namespace (startRun): restart
// reconcile must find it there and leave the still-running trial alone.
func TestSweepReconcileUsesEnvNamespace(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "train-run-1", "namespace": "luncur-ml-staging"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), job)
	s := sweepTestServer(t, dyn)
	p, err := s.st.CreateProject("ml")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	stg, err := s.st.GetEnvironment(p.ID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.st.CreateAppInEnv(stg.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.CreateDeployment(a.ID, "live", "trainer:1", 0); err != nil {
		t.Fatal(err)
	}
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
	if run.ID != 1 {
		t.Fatalf("run id %d, test assumes 1", run.ID)
	}
	if err := s.st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}

	s.sweepReconcile(context.Background())

	got, _ := s.st.ListTrials(sw.ID)
	if got[0].State != "running" {
		t.Fatalf("trial state = %q, want running (Job train-run-1 still exists in luncur-ml-staging)", got[0].State)
	}
}

// A stop that lands while a tick is in flight must not leave a trial
// "running" under a "stopped" sweep (the loop never drives a stopped sweep
// again, so its Job would run untracked to completion).
func TestSweepStopDuringTickLaunchesNothing(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	st := newTestStore(t)
	sealer, _ := secret.New(make([]byte, 32))
	s := newServer(Deps{Store: st, Kube: kube.NewForTest(dyn, k8sfake.NewSimpleClientset()), Sealer: sealer, ExternalIP: "1.2.3.4"})
	p, a := sweepSeedApp(t, s.st)
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	a, _ = s.st.GetAppByID(a.ID)

	sw, _, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 2, Parallel: 2, Nodes: 1,
	}, []string{`{"lr":"0.1"}`, `{"lr":"0.2"}`})
	if err != nil {
		t.Fatal(err)
	}
	// sweepMLflowURLFn runs after the tick has read trials and before it
	// acts on them: fire a concurrent POST .../stop from there and give it
	// a moment to land.
	stopped := make(chan struct{})
	s.sweepMLflowURLFn = func(store.App, string) string {
		go func() {
			defer close(stopped)
			cur, _ := s.st.GetSweep(sw.ID)
			if err := s.stopSweep(context.Background(), cur, a, p); err != nil {
				t.Errorf("stopSweep: %v", err)
			}
		}()
		select {
		case <-stopped:
		case <-time.After(200 * time.Millisecond):
		}
		return ""
	}

	s.sweepTick(context.Background())
	<-stopped

	gotSweep, _ := s.st.GetSweep(sw.ID)
	trials, _ := s.st.ListTrials(sw.ID)
	for _, tr := range trials {
		if tr.State == "running" {
			t.Fatalf("sweep status=%q but trial %s still running after stop", gotSweep.Status, tr.ID)
		}
	}
	if gotSweep.Status != "stopped" {
		t.Fatalf("sweep status = %q, want stopped", gotSweep.Status)
	}
}

// A run whose Job outlived a server restart is recorded once its Job has
// succeeded, so the sweep trial built on it is harvested and the sweep finishes.
func TestRunOrphanedByRestartEventuallyFinishes(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "train-run-1", "namespace": "luncur-ml"},
		"status":   map[string]any{"succeeded": int64(1)},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), job)
	s := sweepTestServer(t, dyn)
	s.sweepMLflowURLFn = func(store.App, string) string { return "" }
	_, a := sweepSeedApp(t, s.st)
	sw, trials, err := s.st.CreateSweep(store.Sweep{
		AppID: a.ID, Metric: "val_loss", Direction: "min", MaxTrials: 1, Parallel: 1, Nodes: 1,
	}, []string{`{"lr":"0.1"}`})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := s.st.CreateJobRun(a.ID, 1, "") // launched by the previous process
	if err := s.st.MarkTrialLaunched(trials[0].ID, run.ID); err != nil {
		t.Fatal(err)
	}

	// Everything startup + the sweep loop does, several ticks' worth.
	ctx := context.Background()
	s.resumeRunWatchers(ctx)
	s.sweepReconcile(ctx)
	for i := 0; i < 3; i++ {
		s.sweepTick(ctx)
	}

	gotRun, _ := s.st.GetJobRun(run.ID)
	gotSweep, _ := s.st.GetSweep(sw.ID)
	if gotRun.Status == "running" || gotSweep.Status == "running" {
		t.Fatalf("Job succeeded but run=%q sweep=%q after restart + 3 ticks", gotRun.Status, gotSweep.Status)
	}
}
