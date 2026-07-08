package server

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/render"
)

// argoWorkflowName is the CR name for a run: "pl-" + runID. runID is a
// nanoid (store.NewID) — already DNS-safe — so no further sanitizing is
// needed, matching render.PipelineStepJobName's "pl-<runID>-..." scheme.
func argoWorkflowName(runID string) string { return "pl-" + runID }

// argoComputeStep is one compute (app/image) pipeline step, fully resolved
// for launch: image/command/env are the caller's job (Task 3's
// startPipelineRun path resolves app steps to their latest live deployment
// image and merges ArtifactEnv + step.Env), so buildWorkflowCR itself stays
// pure and golden-testable.
type argoComputeStep struct {
	Step    pipeline.Step
	Image   string
	Command []string
	Env     map[string]string // fully merged; sorted at build time
}

// argoEnvSlice renders a step's env map as a sorted []{name,value} slice —
// same determinism convention as render.runEnvVars, so the same env map
// always yields byte-identical CR JSON.
func argoEnvSlice(env map[string]string) []any {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"name": k, "value": env[k]})
	}
	return out
}

// filterComputeNeeds keeps only the needs entries that name another
// compute step in this Workflow. Action steps are terminal under the argo
// engine (validated before buildWorkflowCR is ever called — spec's
// 2026-07-08 amendment), so no compute step's needs should ever name one;
// this filter is defense in depth, not the enforcement point.
func filterComputeNeeds(needs []string, computeNames map[string]bool) []string {
	var out []string
	for _, n := range needs {
		if computeNames[n] {
			out = append(out, n)
		}
	}
	return out
}

// buildWorkflowCR compiles a run's compute steps into one Argo Workflow CR:
// a single DAG template ("main") with one task+container-template pair per
// step, dependencies from each step's needs (filtered to compute steps —
// see filterComputeNeeds), retryStrategy when retries>0, and GPU
// scheduling fields when gpu>0.
//
// Sealed app env is NOT inlined into the CR (the values would land in
// etcd); instead an app-kind step's container carries envFrom -> secretRef
// render.SecretName(app) — exactly how render.go's Render mounts app env
// onto the Deployment container (see render.go's `container.EnvFrom`
// wiring). Image-kind steps get plain env only (convention + artifacts +
// step overlay, already merged into cs.Env by the caller).
//
// Every field shape below is an assumption pending a live-cluster field
// test (repo convention: // VERIFY(argo-field) markers).
//
//	apiVersion argoproj.io/v1alpha1, kind Workflow                    // VERIFY(argo-field)
//	metadata: name=argoWorkflowName(runID), namespace, labels
//	  {app.kubernetes.io/managed-by: luncur, luncur.dev/pipeline-run: runID}
//	spec.entrypoint: "main"
//	spec.templates[0] = {name: "main", dag: {tasks: [{name, template: "t-"+name,
//	  dependencies: [...]}]}}                                        // VERIFY(argo-field)
//	per step: {name: "t-"+name, container: {image, command, env: [{name,value}...],
//	  envFrom (app steps only)}, retryStrategy: {limit: "<retries>"} when
//	  retries > 0}                                                    // VERIFY(argo-field)
//	gpu > 0: container.resources.limits/requests "nvidia.com/gpu", plus
//	  template-level runtimeClassName/nodeSelector mirroring
//	  render.applyGPU's pod-spec wiring                               // VERIFY(argo-field)
func buildWorkflowCR(namespace, runID string, steps []argoComputeStep) render.Object {
	computeNames := make(map[string]bool, len(steps))
	for _, cs := range steps {
		computeNames[cs.Step.Name] = true
	}

	tasks := make([]any, 0, len(steps))
	templates := make([]any, 0, len(steps)+1)
	for _, cs := range steps {
		container := map[string]any{"image": cs.Image}
		if len(cs.Command) > 0 {
			cmd := make([]any, len(cs.Command))
			for i, c := range cs.Command {
				cmd[i] = c
			}
			container["command"] = cmd
		}
		if len(cs.Env) > 0 {
			container["env"] = argoEnvSlice(cs.Env)
		}
		if cs.Step.Kind == "app" {
			container["envFrom"] = []any{
				map[string]any{"secretRef": map[string]any{"name": render.SecretName(cs.Step.App)}},
			}
		}
		if cs.Step.GPU > 0 {
			gpu := strconv.Itoa(cs.Step.GPU)
			res := map[string]any{render.GPUResource: gpu}
			container["resources"] = map[string]any{"requests": res, "limits": res}
		}

		tmplName := "t-" + cs.Step.Name
		tmpl := map[string]any{"name": tmplName, "container": container}
		if cs.Step.GPU > 0 {
			tmpl["runtimeClassName"] = render.GPURuntimeClass
			tmpl["nodeSelector"] = map[string]any{render.GPUNodeLabelKey: render.GPUNodeLabelValue}
		}
		if cs.Step.Retries > 0 {
			tmpl["retryStrategy"] = map[string]any{"limit": strconv.Itoa(cs.Step.Retries)}
		}
		templates = append(templates, tmpl)

		task := map[string]any{"name": cs.Step.Name, "template": tmplName}
		if deps := filterComputeNeeds(cs.Step.Needs, computeNames); len(deps) > 0 {
			depsAny := make([]any, len(deps))
			for i, d := range deps {
				depsAny[i] = d
			}
			task["dependencies"] = depsAny
		}
		tasks = append(tasks, task)
	}

	mainTemplate := map[string]any{"name": "main", "dag": map[string]any{"tasks": tasks}}
	allTemplates := append([]any{mainTemplate}, templates...)

	obj := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":      argoWorkflowName(runID),
			"namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "luncur",
				"luncur.dev/pipeline-run":      runID,
			},
		},
		"spec": map[string]any{
			"entrypoint": "main",
			"templates":  allTemplates,
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		// obj is built entirely from this function's own strings/maps/
		// slices — marshal cannot fail in practice.
		panic(err)
	}
	return render.Object{Kind: "Workflow", JSON: b}
}

// argoPhaseToState maps a Workflow node phase to a pipeline_run_steps
// state, so API/CLI output stays engine-agnostic between native and argo
// runs. Unrecognized/empty phases (a node that hasn't reported a phase
// yet) map to "running" rather than guessing done/failed.
// // VERIFY(argo-field): the exact phase string set (Pending/Running/
// Succeeded/Failed/Error/Omitted/Skipped) is assumed from Argo docs.
func argoPhaseToState(phase string) string {
	switch phase {
	case "Pending", "Running":
		return "running"
	case "Succeeded":
		return "done"
	case "Failed", "Error":
		return "failed"
	case "Omitted", "Skipped":
		return "skipped"
	default:
		return "running"
	}
}

// argoRetrySuffixRe strips a retry attempt suffix like "(1)" off a node's
// displayName, collapsing every attempt of a step back onto the step name.
// // VERIFY(argo-field): retry displayName suffix format "<name>(<n>)".
var argoRetrySuffixRe = regexp.MustCompile(`\(\d+\)$`)

// argoNodeStates extracts step name -> pipeline_run_steps state from a
// Workflow CR's status.nodes map (wf is the map[string]any content
// kube.GetWorkflow returns), plus the workflow-level phase from
// status.phase. Nodes of type "Pod" are one step attempt; nodes of type
// "Retry" are that step's retry parent, whose own phase reflects the final
// outcome across every attempt — so a Retry node's phase wins over its
// (possibly stale, e.g. a failed earlier attempt) Pod node when both are
// present for the same displayName. Missing/empty status.nodes -> empty
// map, not an error (a workflow that hasn't started scheduling pods yet).
// // VERIFY(argo-field): node.type values ("Pod"/"Retry"/others), and that
// a Retry node's own phase is authoritative over its children's.
func argoNodeStates(wf map[string]any) (steps map[string]string, wfPhase string) {
	wfPhase, _, _ = unstructured.NestedString(wf, "status", "phase")

	nodes, found, _ := unstructured.NestedMap(wf, "status", "nodes")
	if !found {
		return map[string]string{}, wfPhase
	}

	podPhase := map[string]string{}
	retryPhase := map[string]string{}
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		displayName, _ := node["displayName"].(string)
		nodeType, _ := node["type"].(string)
		phase, _ := node["phase"].(string)
		name := argoRetrySuffixRe.ReplaceAllString(displayName, "")
		switch nodeType {
		case "Pod":
			podPhase[name] = phase
		case "Retry":
			retryPhase[name] = phase
		}
	}

	steps = make(map[string]string, len(podPhase)+len(retryPhase))
	for name, phase := range podPhase {
		steps[name] = argoPhaseToState(phase)
	}
	for name, phase := range retryPhase {
		steps[name] = argoPhaseToState(phase) // Retry parent wins.
	}
	return steps, wfPhase
}
