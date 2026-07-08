package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/store"
)

func TestArgoWorkflowName(t *testing.T) {
	if got, want := argoWorkflowName("abc123defghi"), "pl-abc123defghi"; got != want {
		t.Fatalf("argoWorkflowName = %q, want %q", got, want)
	}
}

// decodeWorkflowCR unmarshals a render.Object's JSON into a generic
// map[string]any tree, the same shape kube.GetWorkflow hands back from the
// dynamic client (unstructured.Unstructured.Object).
func decodeWorkflowCR(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode workflow CR: %v", err)
	}
	return m
}

func nestedMap(t *testing.T, m map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := m
	for _, p := range path {
		v, ok := cur[p]
		if !ok {
			t.Fatalf("missing %q in %+v", p, cur)
		}
		next, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%q not a map: %+v", p, v)
		}
		cur = next
	}
	return cur
}

func nestedSlice(t *testing.T, m map[string]any, path ...string) []any {
	t.Helper()
	cur := nestedMap(t, m, path[:len(path)-1]...)
	v, ok := cur[path[len(path)-1]]
	if !ok {
		t.Fatalf("missing %q in %+v", path[len(path)-1], cur)
	}
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%q not a slice: %+v", path[len(path)-1], v)
	}
	return s
}

func findByName(t *testing.T, items []any, name string) map[string]any {
	t.Helper()
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("no item named %q in %+v", name, items)
	return nil
}

// TestBuildWorkflowCRDiamond covers a two-step DAG (train needs prep):
// name/namespace/labels, the main DAG template's tasks and dependencies,
// per-step templates with sorted env, retryStrategy only when retries>0,
// GPU fields only when gpu>0, and app-kind envFrom secretRef vs.
// image-kind plain env.
func TestBuildWorkflowCRDiamond(t *testing.T) {
	steps := []argoComputeStep{
		{
			Step: pipeline.Step{Name: "prep", Kind: "image"},
			Image: "prep:1",
			Env:   map[string]string{"B": "2", "A": "1"},
		},
		{
			Step: pipeline.Step{
				Name: "train", Kind: "app", App: "worker",
				Needs: []string{"prep"}, GPU: 2, Retries: 3,
			},
			Image:   "worker:latest",
			Command: []string{"python", "train.py"},
			Env:     map[string]string{"X": "y"},
		},
	}

	obj := buildWorkflowCR("proj", "run1", steps)
	if obj.Kind != "Workflow" {
		t.Fatalf("Kind = %q, want Workflow", obj.Kind)
	}
	cr := decodeWorkflowCR(t, obj.JSON)

	if cr["apiVersion"] != "argoproj.io/v1alpha1" || cr["kind"] != "Workflow" {
		t.Fatalf("apiVersion/kind = %v/%v", cr["apiVersion"], cr["kind"])
	}
	meta := nestedMap(t, cr, "metadata")
	if meta["name"] != "pl-run1" {
		t.Fatalf("metadata.name = %v, want pl-run1", meta["name"])
	}
	if meta["namespace"] != "proj" {
		t.Fatalf("metadata.namespace = %v, want proj", meta["namespace"])
	}
	labels := nestedMap(t, cr, "metadata", "labels")
	if labels["app.kubernetes.io/managed-by"] != "luncur" || labels["luncur.dev/pipeline-run"] != "run1" {
		t.Fatalf("labels = %+v", labels)
	}

	spec := nestedMap(t, cr, "spec")
	if spec["entrypoint"] != "main" {
		t.Fatalf("spec.entrypoint = %v, want main", spec["entrypoint"])
	}
	templates := nestedSlice(t, cr, "spec", "templates")

	main := findByName(t, templates, "main")
	dag := nestedMap(t, main, "dag")
	tasks, ok := dag["tasks"].([]any)
	if !ok || len(tasks) != 2 {
		t.Fatalf("dag.tasks = %+v, want 2 tasks", dag["tasks"])
	}

	prepTask := findByName(t, tasks, "prep")
	if prepTask["template"] != "t-prep" {
		t.Fatalf("prep task template = %v, want t-prep", prepTask["template"])
	}
	if _, has := prepTask["dependencies"]; has {
		t.Fatalf("prep task has dependencies, want none: %+v", prepTask)
	}

	trainTask := findByName(t, tasks, "train")
	if trainTask["template"] != "t-train" {
		t.Fatalf("train task template = %v, want t-train", trainTask["template"])
	}
	deps, ok := trainTask["dependencies"].([]any)
	if !ok || len(deps) != 1 || deps[0] != "prep" {
		t.Fatalf("train dependencies = %+v, want [prep]", trainTask["dependencies"])
	}

	// prep template: image env, sorted, no envFrom, no gpu, no retryStrategy.
	prepTmpl := findByName(t, templates, "t-prep")
	prepContainer := nestedMap(t, prepTmpl, "container")
	if prepContainer["image"] != "prep:1" {
		t.Fatalf("prep image = %v", prepContainer["image"])
	}
	prepEnv, ok := prepContainer["env"].([]any)
	if !ok || len(prepEnv) != 2 {
		t.Fatalf("prep env = %+v, want 2 entries", prepContainer["env"])
	}
	if e0 := prepEnv[0].(map[string]any); e0["name"] != "A" || e0["value"] != "1" {
		t.Fatalf("prep env[0] = %+v, want A=1 (sorted)", e0)
	}
	if e1 := prepEnv[1].(map[string]any); e1["name"] != "B" || e1["value"] != "2" {
		t.Fatalf("prep env[1] = %+v, want B=2 (sorted)", e1)
	}
	if _, has := prepContainer["envFrom"]; has {
		t.Fatalf("prep container has envFrom, want none (image kind): %+v", prepContainer)
	}
	if _, has := prepTmpl["retryStrategy"]; has {
		t.Fatalf("prep template has retryStrategy, want none (retries=0): %+v", prepTmpl)
	}
	if _, has := prepTmpl["nodeSelector"]; has {
		t.Fatalf("prep template has nodeSelector, want none (gpu=0): %+v", prepTmpl)
	}

	// train template: app envFrom secretRef, command, gpu resources +
	// runtimeClassName/nodeSelector, retryStrategy.limit.
	trainTmpl := findByName(t, templates, "t-train")
	trainContainer := nestedMap(t, trainTmpl, "container")
	if trainContainer["image"] != "worker:latest" {
		t.Fatalf("train image = %v", trainContainer["image"])
	}
	cmd, ok := trainContainer["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[0] != "python" || cmd[1] != "train.py" {
		t.Fatalf("train command = %+v", trainContainer["command"])
	}
	envFrom, ok := trainContainer["envFrom"].([]any)
	if !ok || len(envFrom) != 1 {
		t.Fatalf("train envFrom = %+v", trainContainer["envFrom"])
	}
	secretRef := nestedMap(t, envFrom[0].(map[string]any), "secretRef")
	if secretRef["name"] != "worker-env" {
		t.Fatalf("train envFrom secretRef.name = %v, want worker-env", secretRef["name"])
	}
	resources := nestedMap(t, trainContainer, "resources")
	limits := nestedMap(t, resources, "limits")
	requests := nestedMap(t, resources, "requests")
	if limits["nvidia.com/gpu"] != "2" || requests["nvidia.com/gpu"] != "2" {
		t.Fatalf("train gpu resources = %+v", resources)
	}
	if trainTmpl["runtimeClassName"] != "nvidia" {
		t.Fatalf("train runtimeClassName = %v, want nvidia", trainTmpl["runtimeClassName"])
	}
	nodeSel := nestedMap(t, trainTmpl, "nodeSelector")
	if nodeSel["luncur.dev/gpu"] != "true" {
		t.Fatalf("train nodeSelector = %+v", nodeSel)
	}
	retryStrategy := nestedMap(t, trainTmpl, "retryStrategy")
	if retryStrategy["limit"] != "3" {
		t.Fatalf("train retryStrategy.limit = %v, want 3", retryStrategy["limit"])
	}
}

func TestArgoPhaseToState(t *testing.T) {
	cases := map[string]string{
		"Pending":   "running",
		"Running":   "running",
		"Succeeded": "done",
		"Failed":    "failed",
		"Error":     "failed",
		"Omitted":   "skipped",
		"Skipped":   "skipped",
		"":          "running",
		"Weird":     "running",
	}
	for phase, want := range cases {
		if got := argoPhaseToState(phase); got != want {
			t.Errorf("argoPhaseToState(%q) = %q, want %q", phase, got, want)
		}
	}
}

// argoStatusFixture is a hand-written Workflow status shape.
// // VERIFY(argo-field): node.type values ("Pod"/"Retry"), displayName
// retry suffix format "<name>(<n>)" — unconfirmed against a live Argo
// controller, field test required.
const argoStatusFixture = `{
	"status": {
		"phase": "Running",
		"nodes": {
			"wf-1": {"displayName": "prep", "type": "Pod", "phase": "Succeeded"},
			"wf-2": {"displayName": "train(0)", "type": "Pod", "phase": "Failed"},
			"wf-3": {"displayName": "train(1)", "type": "Pod", "phase": "Succeeded"},
			"wf-4": {"displayName": "train", "type": "Retry", "phase": "Succeeded"}
		}
	}
}`

func TestArgoNodeStatesCollapsesRetries(t *testing.T) {
	wf := decodeWorkflowCR(t, []byte(argoStatusFixture))
	steps, wfPhase := argoNodeStates(wf)
	if wfPhase != "Running" {
		t.Fatalf("wfPhase = %q, want Running", wfPhase)
	}
	if steps["prep"] != "done" {
		t.Fatalf("steps[prep] = %q, want done", steps["prep"])
	}
	// The Retry parent's own phase (Succeeded) wins over its failed first
	// attempt's Pod node.
	if steps["train"] != "done" {
		t.Fatalf("steps[train] = %q, want done (retry parent wins)", steps["train"])
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want 2 entries", steps)
	}
}

func TestArgoNodeStatesEmptyStatus(t *testing.T) {
	steps, wfPhase := argoNodeStates(map[string]any{})
	if wfPhase != "" {
		t.Fatalf("wfPhase = %q, want empty", wfPhase)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want empty", steps)
	}
}

func TestArgoNodeStatesMissingNodes(t *testing.T) {
	wf := decodeWorkflowCR(t, []byte(`{"status": {"phase": "Succeeded"}}`))
	steps, wfPhase := argoNodeStates(wf)
	if wfPhase != "Succeeded" {
		t.Fatalf("wfPhase = %q, want Succeeded", wfPhase)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want empty", steps)
	}
}

// --- Task 3: engine wiring test helpers -------------------------------

// pipelineSeedArgoPipeline is pipelineSeedPipeline's argo-engine twin
// (pipelines_test.go).
func pipelineSeedArgoPipeline(t *testing.T, st *store.Store, projectID int64, name, yamlStr string) store.Pipeline {
	t.Helper()
	pl, err := st.CreatePipeline(store.Pipeline{ProjectID: projectID, Name: name, YAML: yamlStr, Engine: "argo"})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// pipelineSeedArgoRun seeds a run whose spec_json is the argo-engine
// envelope (pipelines_test.go's pipelineSeedRun writes the bare/native
// shape) — for tests that drive pipelineTick/stopPipelineRun directly
// without going through startPipelineRun's preflight+apply.
func pipelineSeedArgoRun(t *testing.T, st *store.Store, pl store.Pipeline, steps []pipeline.Step) store.PipelineRun {
	t.Helper()
	b, err := json.Marshal(struct {
		Engine string        `json:"engine"`
		Spec   pipeline.Spec `json:"spec"`
	}{Engine: "argo", Spec: pipeline.Spec{Steps: steps}})
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

// argoApplyingDyn is a fake dynamic client whose "patch" reactor for the
// workflows resource actually persists the applied object into the
// tracker, working around the fake's documented SSA-merge limitation for
// unstructured/empty-scheme kinds (kube_test.go's
// TestApplyWorkflowPatchesTheWorkflowGVR: the tracker's default apply-patch
// handling can't merge a type it has no registered struct for — and this
// package's test scheme registers nothing, so even EnsureNamespace's plain
// Namespace patch would hit the same wall). Every non-patch verb (including
// plain Get, used by GetWorkflow/HasWorkflowCRD) falls through to the
// tracker's normal, real behavior — so a test using this fake can Apply a
// Workflow (and EnsureNamespace) via kube.Apply and then really read them
// back, unlike a blanket-success recorder.
func argoApplyingDyn(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa := a.(ktesting.PatchAction)
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), &obj.Object); err != nil {
			return true, nil, err
		}
		obj.SetName(pa.GetName())
		obj.SetNamespace(pa.GetNamespace())
		gvr := pa.GetResource()
		if err := dyn.Tracker().Create(gvr, obj, pa.GetNamespace()); err != nil {
			if uerr := dyn.Tracker().Update(gvr, obj, pa.GetNamespace()); uerr != nil {
				return true, nil, uerr
			}
		}
		return true, obj, nil
	})
	return dyn
}

// --- Task 3: start preflight --------------------------------------------

// TestArgoStartRejectedWithoutCRD covers the CRD preflight: a plain fake
// dynamic client (real tracker, nothing seeded) reports the Argo Workflows
// CRD absent, and startPipelineRun must 400 (errArgoNotInstalled) before
// touching the store any further.
func TestArgoStartRejectedWithoutCRD(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe", "steps:\n  train:\n    app: train\n")

	_, _, err := s.startPipelineRun(context.Background(), pl, "manual")
	if !errors.Is(err, errArgoNotInstalled) {
		t.Fatalf("start without CRD: err = %v, want errArgoNotInstalled", err)
	}
}

// TestArgoStartRejectsComputeAfterAction covers the terminal-actions
// validation (spec's 2026-07-08 amendment): a compute step needing an
// action step must 400 naming both steps. recordingDyn's blanket-success
// reactor reads as "CRD present" (kube.HasWorkflowCRD's nil-Get doc
// comment), so this test reaches past the CRD gate to the DAG check.
func TestArgoStartRejectsComputeAfterAction(t *testing.T) {
	dyn := recordingDyn(t)
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe",
		"steps:\n  note:\n    notify: hi\n  train:\n    app: train\n    needs:\n      - note\n")

	_, _, err := s.startPipelineRun(context.Background(), pl, "manual")
	if !errors.Is(err, errBadPipelineRequest) {
		t.Fatalf("compute-after-action: err = %v, want errBadPipelineRequest", err)
	}
	if !strings.Contains(err.Error(), "train") || !strings.Contains(err.Error(), "note") {
		t.Fatalf("error must name both steps: %v", err)
	}
}

// TestArgoStartRejectsOverBudget covers the static whole-DAG GPU budget
// check: an image step declaring more gpu than the project's quota must
// 400, before any run/step state is created.
func TestArgoStartRejectsOverBudget(t *testing.T) {
	dyn := recordingDyn(t)
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	if err := s.st.SetProjectGPUQuota(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe", "steps:\n  train:\n    image: trainer:1\n    gpu: 2\n")

	_, _, err := s.startPipelineRun(context.Background(), pl, "manual")
	if !errors.Is(err, errBadPipelineRequest) {
		t.Fatalf("over budget: err = %v, want errBadPipelineRequest", err)
	}
}

// TestArgoStartAppliesWorkflowAndMarksRunning covers the happy start path
// end to end: CRD present, DAG/budget/image resolution all pass, the
// Workflow CR is really applied (argoApplyingDyn round-trips it, unlike a
// blanket-success recorder), the run's spec_json envelope records
// engine=argo, and the compute step row is marked running with attempt 0
// and no job_run_id (Argo owns it, not job_runs) — and stays running
// through the immediate post-start tick, since the freshly-applied
// Workflow has no populated status yet (argoNodeStates reports nothing for
// it, so the tick makes no transition).
func TestArgoStartAppliesWorkflowAndMarksRunning(t *testing.T) {
	dyn := argoApplyingDyn(t)
	// HasWorkflowCRD preflights on a real Get for the CRD object — seed it
	// directly into the tracker (kube_test.go's TestHasWorkflowCRDPresent
	// fixture) so the preflight actually passes under this real (non
	// blanket-success) fake.
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "workflows.argoproj.io"},
	}}
	if err := dyn.Tracker().Add(crd); err != nil {
		t.Fatal(err)
	}
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pipelineSeedApp(t, s.st, p.ID, "train", "job", "trainer:1")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe", "steps:\n  train:\n    app: train\n")

	run, steps, err := s.startPipelineRun(context.Background(), pl, "manual")
	if err != nil {
		t.Fatalf("start argo run: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("run status = %q, want running", run.Status)
	}
	if len(steps) != 1 || steps[0].State != "running" || steps[0].JobRunID.Valid {
		t.Fatalf("step = %+v, want running with no job_run_id", steps[0])
	}
	if steps[0].Attempt != 0 {
		t.Fatalf("step attempt = %d, want 0 (MarkStepRunning(nil, 0))", steps[0].Attempt)
	}

	wf, found, err := s.kube.GetWorkflow(context.Background(), p.Namespace, argoWorkflowName(run.ID))
	if err != nil || !found {
		t.Fatalf("GetWorkflow after start = (found=%v, err=%v), want found", found, err)
	}
	if wf["kind"] != "Workflow" {
		t.Fatalf("applied object kind = %v, want Workflow", wf["kind"])
	}

	_, engine, err := decodePipelineRunSpec(run.SpecJSON)
	if err != nil || engine != "argo" {
		t.Fatalf("decodePipelineRunSpec engine = %q, err = %v, want argo", engine, err)
	}
}

// --- Task 3: tick ---------------------------------------------------------

// argoWorkflowStatusObj builds an unstructured Workflow CR with a canned
// status.nodes shape, for seeding directly into a fake dynamic tracker
// (dyn.Tracker().Add) so GetWorkflow reads real content without going
// through kube.Apply's SSA-merge limitation.
func argoWorkflowStatusObj(name, namespace, phase string, nodes map[string]map[string]string) *unstructured.Unstructured {
	nodesMap := map[string]any{}
	for id, n := range nodes {
		nodesMap[id] = map[string]any{
			"displayName": n["displayName"],
			"type":        n["type"],
			"phase":       n["phase"],
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"status":   map[string]any{"phase": phase, "nodes": nodesMap},
	}}
}

// TestArgoTickMapsNodeStatesToSteps covers the core tick mapping: a
// Succeeded Pod node finishes its step done, a Failed one finishes it
// failed, each with detail "argo node <state>".
func TestArgoTickMapsNodeStatesToSteps(t *testing.T) {
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
	rowA := pipelineFindStep(t, s.st, run.ID, "a")
	rowB := pipelineFindStep(t, s.st, run.ID, "b")
	if err := s.st.MarkStepRunning(rowA.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.st.MarkStepRunning(rowB.ID, nil, 0); err != nil {
		t.Fatal(err)
	}

	wf := argoWorkflowStatusObj(argoWorkflowName(run.ID), p.Namespace, "Running", map[string]map[string]string{
		"n1": {"displayName": "a", "type": "Pod", "phase": "Succeeded"},
		"n2": {"displayName": "b", "type": "Pod", "phase": "Failed"},
	})
	if err := dyn.Tracker().Add(wf); err != nil {
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	gotA := pipelineFindStep(t, s.st, run.ID, "a")
	if gotA.State != "done" || gotA.Detail != "argo node done" {
		t.Fatalf("step a = %+v, want done/argo node done", gotA)
	}
	gotB := pipelineFindStep(t, s.st, run.ID, "b")
	if gotB.State != "failed" || gotB.Detail != "argo node failed" {
		t.Fatalf("step b = %+v, want failed/argo node failed", gotB)
	}
}

// TestArgoTickWorkflowMissingFailsRunningRowsAndWarns covers the vanished-
// Workflow branch: every running compute row fails with detail "argo node
// missing", and the run gets a sticky one-time warning.
func TestArgoTickWorkflowMissingFailsRunningRowsAndWarns(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()) // nothing seeded
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "ml")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe", "steps:\n  a:\n    image: busybox\n")
	run := pipelineSeedArgoRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "image", Image: "busybox"}})
	rowA := pipelineFindStep(t, s.st, run.ID, "a")
	if err := s.st.MarkStepRunning(rowA.ID, nil, 0); err != nil {
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	gotA := pipelineFindStep(t, s.st, run.ID, "a")
	if gotA.State != "failed" || gotA.Detail != "argo node missing" {
		t.Fatalf("step a = %+v, want failed/argo node missing", gotA)
	}
	gotRun, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Warning != "argo workflow missing" {
		t.Fatalf("run warning = %q, want %q", gotRun.Warning, "argo workflow missing")
	}
	if gotRun.Status != "failed" {
		t.Fatalf("run status = %q, want failed", gotRun.Status)
	}
}

// TestArgoTickNotifyActionFiresAfterComputeDone covers the terminal-action
// launch path: a notify step whose only need is an already-done compute
// step fires (reusing the native action path) and finishes the step.
func TestArgoTickNotifyActionFiresAfterComputeDone(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	s := pipelineTestServer(t, dyn, nil)
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)
	setSealedNotifyURL(t, s, ts.URL)
	if err := s.st.SetSetting("notify_events", "pipeline"); err != nil {
		t.Fatal(err)
	}

	p := pipelineSeedProject(t, s.st, "ml")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe",
		"steps:\n  a:\n    image: busybox\n  n:\n    notify: done training\n    needs:\n      - a\n")
	run := pipelineSeedArgoRun(t, s.st, pl, []pipeline.Step{
		{Name: "a", Kind: "image", Image: "busybox"},
		{Name: "n", Kind: "notify", Notify: "done training", Needs: []string{"a"}},
	})
	rowA := pipelineFindStep(t, s.st, run.ID, "a")
	if err := s.st.MarkStepRunning(rowA.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.st.FinishStep(rowA.ID, "done", "argo node done"); err != nil {
		t.Fatal(err)
	}

	s.pipelineTick(context.Background())

	gotN := pipelineFindStep(t, s.st, run.ID, "n")
	if gotN.State != "done" || gotN.Detail != "notified" {
		t.Fatalf("step n = %+v, want done/notified", gotN)
	}
	b := recvNotify(t, ch, 2*time.Second)
	if !bytesContains(b, "done training") {
		t.Fatalf("notify body = %s", b)
	}
}

// --- Task 3: stop ----------------------------------------------------------

// TestArgoStopDeletesWorkflow covers stopPipelineRun's argo branch:
// DeleteWorkflow fires alongside the existing running/pending state sweep.
func TestArgoStopDeletesWorkflow(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	// cs (typed clientset) must be non-nil: the step is kind=image, and
	// stopPipelineRun's existing sweep calls kube.DeleteJob for running
	// image rows regardless of engine (harmless no-op here — this run
	// never created a native Job), which needs the typed half wired
	// (TestStopPipelineRunKillsRunningAndSkipsPending's convention).
	cs := k8sfake.NewSimpleClientset()
	s := pipelineTestServer(t, dyn, cs)
	p := pipelineSeedProject(t, s.st, "ml")
	pl := pipelineSeedArgoPipeline(t, s.st, p.ID, "pipe", "steps:\n  a:\n    image: busybox\n")
	run := pipelineSeedArgoRun(t, s.st, pl, []pipeline.Step{{Name: "a", Kind: "image", Image: "busybox"}})
	rowA := pipelineFindStep(t, s.st, run.ID, "a")
	if err := s.st.MarkStepRunning(rowA.ID, nil, 0); err != nil {
		t.Fatal(err)
	}

	got, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.stopPipelineRun(context.Background(), got); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, a := range actions {
		if a == "delete workflows" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no delete workflows action recorded: %v", actions)
	}
	gotA := pipelineFindStep(t, s.st, run.ID, "a")
	if gotA.State != "failed" || gotA.Detail != "stopped" {
		t.Fatalf("step a = %+v, want failed/stopped", gotA)
	}
	gotRun, err := s.st.GetPipelineRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != "stopped" {
		t.Fatalf("run status = %q, want stopped", gotRun.Status)
	}
}
