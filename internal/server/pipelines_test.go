package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/secret"
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

// --- pipelineStepEnv (pure) --------------------------------------------

func TestPipelineStepEnvStepEnvOverlaysArtifactEnv(t *testing.T) {
	pl := store.Pipeline{Name: "pl"}
	st := pipeline.Step{
		Name: "train", Kind: "app", Outputs: []string{"model"},
		Env: map[string]string{"LUNCUR_PIPELINE_ID": "overridden", "LR": "0.1"},
	}
	env := pipelineStepEnv(pl, "run1", st)
	if env["LUNCUR_PIPELINE_ID"] != "overridden" {
		t.Fatalf("step env must win over ArtifactEnv, got %q", env["LUNCUR_PIPELINE_ID"])
	}
	if env["LR"] != "0.1" {
		t.Fatalf("step env LR missing: %+v", env)
	}
	if env["LUNCUR_OUTPUT_MODEL"] != "pipelines/pl/run1/train/model" {
		t.Fatalf("artifact env missing: %+v", env)
	}
}

// --- engine loop test helpers --------------------------------------------

// pipelineTestServer builds a bare *server for exercising the pipeline
// engine's unexported methods directly (sweepTestServer's pattern). dyn/cs
// may both be nil for tests that never reach a kube call.
func pipelineTestServer(t *testing.T, dyn *dynamicfake.FakeDynamicClient, cs kubernetes.Interface) *server {
	t.Helper()
	st := newTestStore(t)
	var kc *kube.Client
	if dyn != nil {
		kc = kube.NewForTest(dyn, cs)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return newServer(Deps{Store: st, Kube: kc, Sealer: sealer, ExternalIP: "1.2.3.4"})
}

// recordingDyn is a fake dynamic client that records nothing and answers
// every action with unconditional success (handled, nil object, nil error)
// — enough for Apply/EnsureNamespace to succeed without a scheme or seeded
// objects (mirrors runs_test.go's kubeServerWithPods).
func recordingDyn(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	return dyn
}

func pipelineSeedProject(t *testing.T, st *store.Store, name string) store.Project {
	t.Helper()
	p, err := st.CreateProject(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// pipelineSeedApp creates an app of the given kind, optionally with a live
// deployment (image != ""). kind=job apps take no port (mirrors
// sweepSeedApp in sweeps_test.go).
func pipelineSeedApp(t *testing.T, st *store.Store, projectID int64, name, kind, image string) store.App {
	t.Helper()
	port := 8080
	if kind == "job" {
		port = 0
	}
	a, err := st.CreateApp(projectID, name, port, kind, "")
	if err != nil {
		t.Fatal(err)
	}
	if image != "" {
		if _, err := st.CreateDeployment(a.ID, "live", image, 0); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func pipelineSeedPipeline(t *testing.T, st *store.Store, projectID int64, name string) store.Pipeline {
	t.Helper()
	pl, err := st.CreatePipeline(store.Pipeline{ProjectID: projectID, Name: name, YAML: "steps:\n  a:\n    app: x\n"})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func pipelineSeedRun(t *testing.T, st *store.Store, pl store.Pipeline, steps []pipeline.Step) store.PipelineRun {
	t.Helper()
	b, err := json.Marshal(pipeline.Spec{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	nk := make([][2]string, len(steps))
	for i, s := range steps {
		nk[i] = [2]string{s.Name, s.Kind}
	}
	run, _, err := st.CreatePipelineRun(store.PipelineRun{PipelineID: pl.ID, SpecJSON: string(b), Trigger: "manual"}, nk)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// pipelineFindStep returns the run's step row named name.
func pipelineFindStep(t *testing.T, st *store.Store, runID, name string) store.PipelineRunStep {
	t.Helper()
	steps, err := st.ListRunSteps(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in run %s", name, runID)
	return store.PipelineRunStep{}
}

// --- loop-level tests (store + fakes) ------------------------------------

func TestPipelineTickLaunchesRootAppStep(t *testing.T) {
	dyn := recordingDyn(t)
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{
		{Name: "a", Kind: "app", App: "train"},
	})

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "a")
	if got.State != "running" || !got.JobRunID.Valid || got.Attempt != 1 {
		t.Fatalf("step a = %+v, want running/attempt=1 with a job_run_id", got)
	}
}

func TestPipelineTickHarvestDoneFinishesRun(t *testing.T) {
	s := pipelineTestServer(t, nil, nil) // no launch needed: only step is already running
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train"}})

	jr, err := s.st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.FinishJobRun(jr.ID, "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	row := pipelineFindStep(t, s.st, run.ID, "a")
	jrID := jr.ID
	if err := s.st.MarkStepRunning(row.ID, &jrID, 1); err != nil {
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "a")
	if got.State != "done" || got.Detail != "exit 0" {
		t.Fatalf("step a = %+v, want done/exit 0", got)
	}
	gotRun, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "done" {
		t.Fatalf("run status = %q, want done (drained same tick)", gotRun.Status)
	}
}

func TestPipelineTickAppRetryRelaunchesUnderRetryLimit(t *testing.T) {
	dyn := recordingDyn(t)
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train", Retries: 2}})

	jr1, err := s.st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.FinishJobRun(jr1.ID, "failed", nil); err != nil {
		t.Fatal(err)
	}
	row := pipelineFindStep(t, s.st, run.ID, "a")
	jr1ID := jr1.ID
	if err := s.st.MarkStepRunning(row.ID, &jr1ID, 1); err != nil {
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "a")
	if got.State != "running" {
		t.Fatalf("step a state = %q, want running (retried, attempt 1 < retries 2)", got.State)
	}
	if got.Attempt != 2 {
		t.Fatalf("step a attempt = %d, want 2", got.Attempt)
	}
	if !got.JobRunID.Valid || got.JobRunID.Int64 == jr1.ID {
		t.Fatalf("step a job_run_id = %+v, want a new job run distinct from %d", got.JobRunID, jr1.ID)
	}
}

func TestPipelineTickAppRetryExhaustedFailsStep(t *testing.T) {
	s := pipelineTestServer(t, nil, nil) // exhausted -> FinishStep directly, no relaunch, no kube needed
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train", Retries: 2}})

	jr, err := s.st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	exitCode := int64(1)
	if err := s.st.FinishJobRun(jr.ID, "failed", &exitCode); err != nil {
		t.Fatal(err)
	}
	row := pipelineFindStep(t, s.st, run.ID, "a")
	jrID := jr.ID
	if err := s.st.MarkStepRunning(row.ID, &jrID, 2); err != nil { // already at attempt 2 == retries
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "a")
	if got.State != "failed" || got.Detail != "exit 1" {
		t.Fatalf("step a = %+v, want failed/exit 1 (attempt == retries, no more relaunches)", got)
	}
	gotRun, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "failed" {
		t.Fatalf("run status = %q, want failed", gotRun.Status)
	}
}

func TestPipelineTickFailFastSkipsDownstreamKeepsSibling(t *testing.T) {
	s := pipelineTestServer(t, nil, nil) // both siblings unreachable/skipped -> no kube needed
	p := pipelineSeedProject(t, s.st, "ml")
	pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{
		{Name: "a", Kind: "app", App: "train"},
		{Name: "b", Kind: "app", App: "train", Needs: []string{"a"}},
	})

	rowA := pipelineFindStep(t, s.st, run.ID, "a")
	if err := s.st.FinishStep(rowA.ID, "failed", "boom"); err != nil {
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	gotB := pipelineFindStep(t, s.st, run.ID, "b")
	if gotB.State != "skipped" || gotB.Detail != "upstream failed" {
		t.Fatalf("step b = %+v, want skipped/upstream failed", gotB)
	}
	gotRun, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// decidePipelineRun's Finish only fires once a later tick observes b
	// already skipped — this same tick's decide input still saw b "pending"
	// (see decide's doc comment / TestDecidePipelineRunFinishEmptyWhileSkipPending).
	if gotRun.Status != "running" {
		t.Fatalf("run status = %q, want still running (b's skip lands this tick, drain observed next tick)", gotRun.Status)
	}

	s.pipelineTick(context.Background())

	gotRun, err = s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "failed" {
		t.Fatalf("run status = %q, want failed (a failed, b skipped, nothing left)", gotRun.Status)
	}
}

func TestPipelineTickDeployActionRedeploysLiveImage(t *testing.T) {
	dyn := recordingDyn(t)
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "api", "web", "api:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "d", Kind: "deploy", Deploy: "api"}})

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "d")
	if got.State != "done" || got.Detail != "deployed api:1" {
		t.Fatalf("step d = %+v, want done/deployed api:1", got)
	}
	deploys, err := s.st.ListDeployments(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deploys) != 2 || deploys[0].Status != "live" || deploys[0].ImageRef != "api:1" {
		t.Fatalf("deployments = %+v, want a second live api:1 deploy", deploys)
	}
}

func TestPipelineTickScaleActionSetsReplicas(t *testing.T) {
	s := pipelineTestServer(t, nil, nil) // app has no live deployment -> scaleApp never touches kube
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "web1", "web", "")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{
		{Name: "s", Kind: "scale", Scale: &pipeline.ScaleAction{App: "web1", Replicas: 3}},
	})

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "s")
	if got.State != "done" || got.Detail != "scaled web1 to 3 replicas" {
		t.Fatalf("step s = %+v, want done/scaled web1 to 3 replicas", got)
	}
	gotApp, err := s.st.GetApp(p.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Replicas != 3 {
		t.Fatalf("app replicas = %d, want 3", gotApp.Replicas)
	}
}

func TestPipelineTickNotifyActionFiresAndFinishes(t *testing.T) {
	s := pipelineTestServer(t, nil, nil)
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)
	setSealedNotifyURL(t, s, ts.URL)
	if err := s.st.SetSetting("notify_events", "pipeline"); err != nil {
		t.Fatal(err)
	}

	p := pipelineSeedProject(t, s.st, "ml")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "n", Kind: "notify", Notify: "hello team"}})

	s.pipelineTick(context.Background())

	got := pipelineFindStep(t, s.st, run.ID, "n")
	if got.State != "done" || got.Detail != "notified" {
		t.Fatalf("step n = %+v, want done/notified", got)
	}
	b := recvNotify(t, ch, 2*time.Second)
	if !bytesContains(b, "hello team") || !bytesContains(b, `"event":"pipeline"`) {
		t.Fatalf("notify body = %s", b)
	}
}

func TestStopPipelineRunKillsRunningAndSkipsPending(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	dyn := recordingDyn(t)
	s := pipelineTestServer(t, dyn, cs)
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{
		{Name: "a", Kind: "app", App: "train"},
		{Name: "b", Kind: "app", App: "train", Needs: []string{"a"}},
	})

	jr, err := s.st.CreateJobRun(a.ID, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	rowA := pipelineFindStep(t, s.st, run.ID, "a")
	jrID := jr.ID
	if err := s.st.MarkStepRunning(rowA.ID, &jrID, 1); err != nil {
		t.Fatal(err)
	}

	got, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.stopPipelineRun(context.Background(), got); err != nil {
		t.Fatal(err)
	}

	gotA := pipelineFindStep(t, s.st, run.ID, "a")
	if gotA.State != "failed" || gotA.Detail != "stopped" {
		t.Fatalf("step a = %+v, want failed/stopped", gotA)
	}
	gotB := pipelineFindStep(t, s.st, run.ID, "b")
	if gotB.State != "skipped" || gotB.Detail != "stopped" {
		t.Fatalf("step b = %+v, want skipped/stopped", gotB)
	}
	gotJR, err := s.st.GetJobRun(jr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotJR.Status != "failed" {
		t.Fatalf("job run status = %q, want failed", gotJR.Status)
	}
	gotRun, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "stopped" {
		t.Fatalf("run status = %q, want stopped", gotRun.Status)
	}
}

func TestPipelineReconcileMarksOrphanedAppStepFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme) // no Job objects seeded -> JobExists is false
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	a := pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "app", App: "train"}})

	jr, err := s.st.CreateJobRun(a.ID, 1, "") // status stays "running"
	if err != nil {
		t.Fatal(err)
	}
	row := pipelineFindStep(t, s.st, run.ID, "a")
	jrID := jr.ID
	if err := s.st.MarkStepRunning(row.ID, &jrID, 1); err != nil {
		t.Fatal(err)
	}

	s.pipelineReconcile(context.Background())

	gotJR, err := s.st.GetJobRun(jr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotJR.Status != "failed" {
		t.Fatalf("job run status = %q, want failed (orphaned, job gone)", gotJR.Status)
	}
	gotStep := pipelineFindStep(t, s.st, run.ID, "a")
	if gotStep.State != "failed" || gotStep.Detail != "job missing after restart" {
		t.Fatalf("step a = %+v, want failed/job missing after restart", gotStep)
	}
}

func TestPipelineReconcileMarksOrphanedImageStepFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme) // no Job objects seeded -> JobExists is false
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "i", Kind: "image", Image: "busybox"}})

	row := pipelineFindStep(t, s.st, run.ID, "i")
	if err := s.st.MarkStepRunning(row.ID, nil, 1); err != nil {
		t.Fatal(err)
	}

	s.pipelineReconcile(context.Background())

	gotStep := pipelineFindStep(t, s.st, run.ID, "i")
	if gotStep.State != "failed" || gotStep.Detail != "job missing after restart" {
		t.Fatalf("step i = %+v, want failed/job missing after restart", gotStep)
	}
}

// --- HTTP endpoint tests (Task 6) ------------------------------------------

// TestPipelinesAPI covers the pipeline CRUD + run lifecycle end to end
// (TestRunsAPI's pattern): create happy path, app-ref validation errors, run
// launching its root step instantly, deleting a busy pipeline, idempotent
// stop, and topo-ordered step output.
func TestPipelinesAPI(t *testing.T) {
	// kubeServerWithPods (not kubeServer): stopping the run below calls
	// kube.DeleteJob, which needs the typed clientset half wired (nil cs
	// panics), unlike a plain create/list flow.
	srv, st, _ := kubeServerWithPods(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	// create with an app: step naming a non-job app -> 400 kind_mismatch.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines", admin,
		`{"name":"bad","yaml":"steps:\n  s:\n    app: api\n"}`)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, mustReadBody(t, resp)) != "kind_mismatch" {
		t.Fatalf("web-app ref: want 400 kind_mismatch, got %d", resp.StatusCode)
	}

	// create with unparseable yaml (two kinds on one step) -> 400 bad_request
	// naming the offending step.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines", admin,
		`{"name":"bad2","yaml":"steps:\n  s:\n    app: train\n    image: busybox\n"}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, body) != "bad_request" {
		t.Fatalf("bad yaml: want 400 bad_request, got %d (%s)", resp.StatusCode, body)
	}
	if !bytesContains(body, `step \"s\"`) {
		t.Fatalf("bad yaml error must name the offending step: %s", body)
	}

	// create happy path.
	pipelineYAML := `steps:
  train:
    app: train
`
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines", admin,
		`{"name":"pipe","yaml":`+jsonQuote(pipelineYAML)+`}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline: want 201, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created["name"] != "pipe" || created["yaml"] != pipelineYAML {
		t.Fatalf("created pipeline = %+v", created)
	}

	// deploy the app so the run's root app step can actually launch.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// run -> 202 with the root step already launched by the inline tick.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines/pipe/runs", admin, `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start run: want 202, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var run map[string]any
	json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if run["status"] != "running" {
		t.Fatalf("run status = %v, want running", run["status"])
	}
	steps, _ := run["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("run steps = %+v, want 1", steps)
	}
	step0 := steps[0].(map[string]any)
	if step0["name"] != "train" || step0["state"] != "running" || step0["job_run_id"] == nil {
		t.Fatalf("root step = %+v, want running with a job_run_id (instant launch)", step0)
	}
	runID := run["id"].(string)

	// delete while the run is still running -> 409 pipeline_busy.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/ml/pipelines/pipe", admin, "")
	if resp.StatusCode != http.StatusConflict || errCode(t, mustReadBody(t, resp)) != "pipeline_busy" {
		t.Fatalf("delete busy pipeline: want 409 pipeline_busy, got %d", resp.StatusCode)
	}

	// get run: steps come back in topo order (single-step here, but exercises
	// the endpoint's step-list plumbing end to end).
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/pipelines/pipe/runs/"+runID, admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get run: want 200, got %d", resp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	gotSteps, _ := got["steps"].([]any)
	if len(gotSteps) != 1 || gotSteps[0].(map[string]any)["name"] != "train" {
		t.Fatalf("get run steps = %+v", gotSteps)
	}

	// stop: running -> stopped.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines/pipe/runs/"+runID+"/stop", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop run: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var stopped map[string]any
	json.NewDecoder(resp.Body).Decode(&stopped)
	resp.Body.Close()
	if stopped["status"] != "stopped" {
		t.Fatalf("stop run status = %v, want stopped", stopped["status"])
	}

	// stop again: idempotent no-op, still 200/stopped.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines/pipe/runs/"+runID+"/stop", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop run again: want 200, got %d", resp.StatusCode)
	}
	var stoppedAgain map[string]any
	json.NewDecoder(resp.Body).Decode(&stoppedAgain)
	resp.Body.Close()
	if stoppedAgain["status"] != "stopped" {
		t.Fatalf("stop run again status = %v, want stopped", stoppedAgain["status"])
	}

	// now the pipeline can be deleted.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/ml/pipelines/pipe", admin, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete pipeline: want 204, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/pipelines/pipe", admin, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted pipeline: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPipelineRunEngineArgoRejected covers a pipeline pinned to engine=argo:
// run start must 400 engine_unavailable until C3 ships the Argo engine.
func TestPipelineRunEngineArgoRejected(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines", admin,
		`{"name":"pipe","engine":"argo","yaml":"steps:\n  train:\n    app: train\n"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline: want 201, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/pipelines/pipe/runs", admin, `{}`)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, mustReadBody(t, resp)) != "engine_unavailable" {
		t.Fatalf("run with engine=argo: want 400 engine_unavailable, got %d", resp.StatusCode)
	}
}

// TestPipelinesUnknownProjectNotFound covers the existing project-scope
// pattern (requireProject) for a project that doesn't exist.
func TestPipelinesUnknownProjectNotFound(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/nope/pipelines", admin, "")
	if resp.StatusCode != http.StatusNotFound || errCode(t, mustReadBody(t, resp)) != "not_found" {
		t.Fatalf("unknown project: want 404 not_found, got %d", resp.StatusCode)
	}
}

// TestSettingPipelineEngineValidation covers settableKeys["pipeline_engine"]:
// native/argo accepted, anything else rejected.
func TestSettingPipelineEngineValidation(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/pipeline_engine", admin, `{"value":"native"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set native: want 204, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/pipeline_engine", admin, `{"value":"argo"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set argo: want 204, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/pipeline_engine", admin, `{"value":"garbage"}`)
	if resp.StatusCode != http.StatusBadRequest || errCode(t, mustReadBody(t, resp)) != "bad_request" {
		t.Fatalf("set garbage: want 400 bad_request, got %d", resp.StatusCode)
	}
}

// jsonQuote encodes s as a JSON string literal, for embedding raw yaml
// (with newlines) into a hand-written JSON request body.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
