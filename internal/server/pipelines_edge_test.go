package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/store"
)

func jobObject(ns, name string, status map[string]any) *unstructured.Unstructured {
	o := map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": ns},
	}
	if status != nil {
		o["status"] = status
	}
	return &unstructured.Unstructured{Object: o}
}

// An app step's Job lives in the app's environment namespace (startRun):
// restart reconcile must look there, not in the production namespace, before
// declaring the Job missing.
func TestPipelineReconcileUsesEnvNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), jobObject("luncur-ml-staging", "train-run-1", nil))
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	stg, err := s.st.GetEnvironment(p.ID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.st.CreateAppInEnv(stg.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train"}})
	jr, _ := s.st.CreateJobRun(a.ID, 1, "")
	if jr.ID != 1 {
		t.Fatalf("job run id %d, test assumes 1", jr.ID)
	}
	row := pipelineFindStep(t, s.st, run.ID, "a")
	if err := s.st.MarkStepRunning(row.ID, &jr.ID, 1); err != nil {
		t.Fatal(err)
	}

	s.pipelineReconcile(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "a")
	if got.State != "running" {
		t.Fatalf("step = %s/%q, want running (Job still exists in luncur-ml-staging)", got.State, got.Detail)
	}
}

// An app step whose Job outlived a server restart still finishes: the
// restarted server re-attaches run watchers, so the run (and a cron
// pipeline's next fire) isn't blocked forever.
func TestPipelineOrphanedAppStepEventuallyFinishes(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		jobObject("luncur-ml", "train-run-1", map[string]any{"succeeded": int64(1)}))
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train"}})
	jr, _ := s.st.CreateJobRun(a.ID, 1, "")
	row := pipelineFindStep(t, s.st, run.ID, "a")
	if err := s.st.MarkStepRunning(row.ID, &jr.ID, 1); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s.resumeRunWatchers(ctx)
	s.pipelineReconcile(ctx)
	for i := 0; i < 3; i++ {
		s.pipelineTick(ctx)
	}

	gotRun, _ := s.st.GetPipelineRun(run.ID)
	if gotRun.Status == "running" {
		t.Fatalf("Job succeeded but pipeline run still %q after restart + 3 ticks (step %+v)", gotRun.Status, pipelineFindStep(t, s.st, run.ID, "a"))
	}
}

// retries: N means N re-runs after the first attempt on both engines: the
// native engine retries a failed first attempt of a retries: 1 step, matching
// argo's retryStrategy.limit=1.
func TestPipelineRetriesOneRetriesOnceOnNative(t *testing.T) {
	views := []pipeStepView{{
		Row:  store.PipelineRunStep{ID: "r", Name: "r", State: "running", Attempt: 1},
		Spec: pipeline.Step{Name: "r", Kind: "app", Retries: 1},
		Run:  &store.JobRun{Status: "failed"},
	}}
	got := decidePipelineRun(pipeline.Spec{Steps: []pipeline.Step{views[0].Spec}}, views)
	wf := buildWorkflowCR("luncur-ml", "run1", []argoComputeStep{{Step: pipeline.Step{Name: "r", Kind: "image", Image: "x", Retries: 1}}})
	if !strings.Contains(string(wf.JSON), `"retryStrategy":{"limit":"1"}`) {
		t.Fatalf("argo workflow lacks retryStrategy.limit=1: %s", wf.JSON)
	}
	if len(got.Launch) != 1 {
		t.Fatalf("retries: 1, first attempt failed -> Launch = %v, want one retry (argo engine gives one)", got.Launch)
	}
}

// Concurrent pipeline ticks (the loop plus a manual/webhook trigger's inline
// tick) launch a pending step exactly once.
func TestPipelineConcurrentTicksLaunchStepOnce(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	var mu sync.Mutex
	jobPatches := 0
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "jobs" {
			mu.Lock()
			jobPatches++
			first := jobPatches == 1
			mu.Unlock()
			if first {
				// A slow first apply (real API server latency) widens the
				// window in which a second tick still reads the step pending.
				time.Sleep(300 * time.Millisecond)
			}
		}
		return true, nil, nil
	})
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train"}})

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ { // background loop tick + startPipelineRun's inline tick
		wg.Add(1)
		go func() { defer wg.Done(); s.pipelineTick(context.Background()) }()
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()

	runs, _ := s.st.ListJobRuns(a.ID)
	if len(runs) != 1 || jobPatches != 1 {
		t.Fatalf("one app step launched %d job_runs / %d Job applies (step now %+v)", len(runs), jobPatches, pipelineFindStep(t, s.st, run.ID, "a"))
	}
}

// Once the argo Workflow is terminal (Failed), a compute row argo never ran
// (an Omitted node of type Skipped) is resolved and the run finishes.
func TestArgoFailedWorkflowFinishesRun(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe",
		"steps:\n  a:\n    app: train\n  b:\n    image: busybox\n    needs:\n      - a\n")
	run := pipelineSeedArgoRun(t, s.st, pl, []pipeline.Step{
		{Name: "a", Kind: "app", App: "train"},
		{Name: "b", Kind: "image", Image: "busybox", Needs: []string{"a"}},
	})
	for _, n := range []string{"a", "b"} {
		if err := s.st.MarkStepRunning(pipelineFindStep(t, s.st, run.ID, n).ID, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	wf := argoWorkflowStatusObj(argoWorkflowName(run.ID), p.Namespace, "Failed", map[string]map[string]string{
		"n1": {"displayName": "a", "type": "Pod", "phase": "Failed"},
		"n2": {"displayName": "b", "type": "Skipped", "phase": "Omitted"},
	})
	if err := dyn.Tracker().Add(wf); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		s.pipelineTick(context.Background())
	}

	got, _ := s.st.GetPipelineRun(run.ID)
	if got.Status == "running" {
		t.Fatalf("workflow phase Failed but run still running; step b = %+v", pipelineFindStep(t, s.st, run.ID, "b"))
	}
}

// A native image step with gpu: goes through the project GPU budget like app
// steps do; one that can never fit is failed instead of applied (the quota
// would refuse its pod and the step would hang).
func TestPipelineImageStepRespectsGPUBudget(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	applied := 0
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "jobs" {
			applied++
		}
		return true, nil, nil
	})
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	if err := s.st.SetProjectGPUQuota(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "big", Kind: "image", Image: "trainer:1", GPU: 4}})

	s.pipelineTick(context.Background())

	if applied != 0 {
		t.Fatalf("4-GPU image step applied in a 1-GPU-budget project (step %+v)", pipelineFindStep(t, s.st, run.ID, "big"))
	}
}
