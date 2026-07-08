package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sutantodadang/luncur/internal/pipeline"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
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

// --- Task 3: engine wiring (start / tick / stop) ---------------------------

// argoResolvedStep is one compute step's launch-time image/command,
// resolved by argoPreflight before the run row exists, so an unresolvable
// app reference surfaces as a 400 before any state is created (mirrors
// validatePipelineAppRefs's existence check).
// argoResolvedStep is one compute step's launch-time image/command/gpu,
// resolved by argoPreflight before the run row exists, so an unresolvable
// app reference surfaces as a 400 before any state is created (mirrors
// validatePipelineAppRefs's existence check). GPU mirrors the source
// buildWorkflowCR needs to render scheduling fields: for image-kind steps
// that's the compiled step's own gpu field; for app-kind steps
// pipeline.Compile forbids gpu on the YAML step itself (image-only field —
// checkFieldLegality), so it comes from the target app's own GPUCount
// instead (the same value the native engine already schedules that app's
// runs with).
type argoResolvedStep struct {
	Image   string
	Command []string
	GPU     int
}

// argoPreflight validates an argo-engine run before any run/step state is
// created: the Argo Workflows CRD must be installed, no compute step may
// depend on an action step (spec's 2026-07-08 terminal-actions amendment),
// every compute step's image/gpu must be resolvable, and the static
// whole-DAG GPU budget (every compute step's resolved gpu, summed —
// worst-case parallel) must fit the project's quota. Returns the resolved
// steps so startPipelineRun can build the Workflow CR once the run (and its
// runID) exists.
func (s *server) argoPreflight(ctx context.Context, project store.Project, spec pipeline.Spec) (map[string]argoResolvedStep, error) {
	if s.kube == nil {
		return nil, fmt.Errorf("%w: kubernetes is not configured", errArgoNotInstalled)
	}
	has, err := s.kube.HasWorkflowCRD(ctx)
	if err != nil {
		return nil, fmt.Errorf("check argo workflows CRD: %w", err)
	}
	if !has {
		return nil, fmt.Errorf("%w: run `luncur argo install` first", errArgoNotInstalled)
	}
	if err := validateArgoTerminalActions(spec); err != nil {
		return nil, err
	}

	resolved, err := s.resolveArgoStepImages(project, spec)
	if err != nil {
		return nil, err
	}

	var totalGPU int64
	for _, st := range spec.Steps {
		if st.Kind == "app" || st.Kind == "image" {
			totalGPU += int64(resolved[st.Name].GPU)
		}
	}
	if err := s.validateGPUBudget(project, totalGPU); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadPipelineRequest, err)
	}

	return resolved, nil
}

// resolveArgoStepImages resolves every compute step's image/gpu: app steps
// use their target app's latest live deployment image plus the app's own
// GPUCount (the same source pipelineRunDeploy's redeploy action reads for
// the image, and the native engine's startRun already schedules that app's
// runs against for gpu); image steps use the compiled step's own
// Image/Command/GPU.
func (s *server) resolveArgoStepImages(project store.Project, spec pipeline.Spec) (map[string]argoResolvedStep, error) {
	appCache := map[string]store.App{}
	out := make(map[string]argoResolvedStep, len(spec.Steps))
	for _, st := range spec.Steps {
		switch st.Kind {
		case "app":
			a, ok := s.pipelineResolveApp(project, st.App, appCache)
			if !ok {
				return nil, fmt.Errorf("%w: step %q: app %q not found", errBadPipelineRequest, st.Name, st.App)
			}
			live, err := s.st.LatestDeployment(a.ID)
			if err != nil || live.Status != "live" || live.ImageRef == "" {
				return nil, fmt.Errorf("%w: step %q: app %q has no live deployment to run", errBadPipelineRequest, st.Name, st.App)
			}
			out[st.Name] = argoResolvedStep{Image: live.ImageRef, GPU: int(a.GPUCount)}
		case "image":
			out[st.Name] = argoResolvedStep{Image: st.Image, Command: st.Command, GPU: st.GPU}
		}
	}
	return out, nil
}

// validateArgoTerminalActions rejects a compute step (kind app/image) whose
// transitive needs graph includes an action step (deploy/scale/notify) —
// spec's 2026-07-08 amendment: actions are terminal under the argo engine,
// running outside the Workflow CR after their dependencies finish, so a
// compute step (which lives inside the CR) can never depend on one.
func validateArgoTerminalActions(spec pipeline.Spec) error {
	byName := make(map[string]pipeline.Step, len(spec.Steps))
	for _, st := range spec.Steps {
		byName[st.Name] = st
	}
	for _, st := range spec.Steps {
		if st.Kind != "app" && st.Kind != "image" {
			continue
		}
		for _, dep := range argoTransitiveNeeds(st, byName) {
			depStep := byName[dep]
			if depStep.Kind != "app" && depStep.Kind != "image" {
				return fmt.Errorf("%w: step %q (compute) depends on action step %q — actions must be terminal under the argo engine",
					errBadPipelineRequest, st.Name, dep)
			}
		}
	}
	return nil
}

// argoTransitiveNeeds returns the set of step names transitively required
// by st via Needs edges. spec is guaranteed acyclic (pipeline.Compile ran
// cycle detection already), so plain recursion is safe.
func argoTransitiveNeeds(st pipeline.Step, byName map[string]pipeline.Step) []string {
	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		for _, dep := range byName[name].Needs {
			if !seen[dep] {
				seen[dep] = true
				walk(dep)
			}
		}
	}
	walk(st.Name)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out
}

// allRowNeedsDone is allNeedsDone's row-only cousin for the argo tick, which
// has no pipeStepView to work from — an argo run's compute rows are driven
// by argoNodeStates instead of job_runs harvesting, so there's no Run field
// to attach.
func allRowNeedsDone(byName map[string]store.PipelineRunStep, needs []string) bool {
	for _, n := range needs {
		row, ok := byName[n]
		if !ok || row.State != "done" {
			return false
		}
	}
	return true
}

// pipelineTickArgoRun drives one tick of an argo-engine run: map the
// Workflow CR's node states onto compute step rows (a state change ->
// FinishStep, detail "argo node <state>"; the Workflow gone entirely ->
// every non-final compute row fails, with a sticky one-time run warning —
// SetSweepWarning's set-once pattern), launch any action step whose needs
// are all done (reusing the native action path — pipelineLaunchStep), and
// finish the run once nothing is pending/running, same rule pipelineTickOne
// uses for native runs. Mirrors decidePipelineRun's pre-launch snapshot
// timing: Finish is decided from the same view Launch acted on this tick, so
// a run whose last row is an action step needs one more tick after that
// action executes to actually finish — identical lag to the native engine.
func (s *server) pipelineTickArgoRun(ctx context.Context, run store.PipelineRun, pl store.Pipeline, project store.Project, spec pipeline.Spec) {
	rows, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		log.Printf("pipeline run %s: argo tick: list steps: %v", run.ID, err)
		return
	}

	wf, found, err := s.kube.GetWorkflow(ctx, project.Namespace, argoWorkflowName(run.ID))
	if err != nil {
		log.Printf("pipeline run %s: argo tick: get workflow: %v", run.ID, err)
		return
	}
	var nodeStates map[string]string
	if found {
		nodeStates, _ = argoNodeStates(wf)
	} else if run.Warning == "" {
		if err := s.st.SetPipelineRunWarning(run.ID, "argo workflow missing"); err != nil {
			log.Printf("pipeline run %s: argo tick: set warning: %v", run.ID, err)
		}
	}

	byName := make(map[string]store.PipelineRunStep, len(rows))
	for i, row := range rows {
		st, ok := spec.Step(row.Name)
		isCompute := ok && (st.Kind == "app" || st.Kind == "image")
		if !isCompute || row.State != "running" {
			byName[row.Name] = row
			continue
		}
		newState := nodeStates[row.Name]
		if !found {
			newState = "failed"
		}
		if newState != "" && newState != "running" && newState != row.State {
			detail := "argo node missing"
			if found {
				detail = "argo node " + newState
			}
			if err := s.st.FinishStep(row.ID, newState, detail); err != nil {
				log.Printf("pipeline run %s: step %s: finish %s: %v", run.ID, row.Name, newState, err)
			} else {
				row.State = newState
			}
		}
		rows[i] = row
		byName[row.Name] = row
	}

	appCache := map[string]store.App{}
	anyPendingOrRunning, allDone := false, true
	for _, row := range rows {
		st, ok := spec.Step(row.Name)
		if !ok {
			continue
		}
		switch row.State {
		case "pending":
			anyPendingOrRunning = true
			allDone = false
			isAction := st.Kind == "deploy" || st.Kind == "scale" || st.Kind == "notify"
			if isAction && allRowNeedsDone(byName, st.Needs) {
				s.pipelineLaunchStep(ctx, run, pl, project, pipeStepView{Row: row, Spec: st}, appCache)
			}
		case "running":
			anyPendingOrRunning = true
			allDone = false
		case "done":
			// inert, and the only state that keeps allDone true.
		default: // failed, skipped
			allDone = false
		}
	}

	if !anyPendingOrRunning {
		finish := "failed"
		if allDone {
			finish = "done"
		}
		if err := s.st.FinishPipelineRun(run.ID, finish); err != nil {
			log.Printf("pipeline run %s: argo tick: finish run: %v", run.ID, err)
			return
		}
		s.notify(notifyEvent{
			Event: "pipeline", Project: project.Name, App: pl.Name,
			Message: fmt.Sprintf("pipeline %s run %s: %s", pl.Name, run.ID, finish),
		})
	}
}
