package server

import (
	"encoding/json"
	"testing"

	"github.com/sutantodadang/luncur/internal/pipeline"
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
