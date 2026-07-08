// Package pipeline compiles and validates pipeline.yaml into a Spec: a
// topologically sorted list of Steps ready for the server's native
// orchestrator loop to drive. This package is pure — no store/kube/server
// imports — so it is unit-testable without a database or cluster, and can
// be reused verbatim by a future Argo compiler.
//
// Compile performs structural validation only (names, DAG shape, field
// legality per kind, artifact wiring). App existence/kind checks are the
// caller's job — the server has the store, this package does not.
package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// maxYAMLBytes caps pipeline.yaml size (repo-wide convention for
// user-supplied config blobs).
const maxYAMLBytes = 64 * 1024

var (
	// stepNameRe mirrors the DNS-1123-ish name luncur uses elsewhere, but
	// capped at 20 chars: step names embed into K8s Job names
	// ("pl-<runID>-<step>-a<attempt>") which must stay under 63 total.
	stepNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// envKeyRe also governs output/input artifact names (same character
	// class; case is normalized to upper when building env var names).
	envKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// ScaleAction is the "scale" built-in action: set an app's replica count.
type ScaleAction struct {
	App      string `json:"app"`
	Replicas int    `json:"replicas"`
}

// Step is one compiled pipeline.yaml step. Kind is derived by Compile from
// exactly one of App/Image/Deploy/Scale/Notify being set.
type Step struct {
	Name    string            `json:"name"`
	Needs   []string          `json:"needs,omitempty"`
	App     string            `json:"app,omitempty"`
	Image   string            `json:"image,omitempty"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	GPU     int               `json:"gpu,omitempty"`
	Deploy  string            `json:"deploy,omitempty"`
	Scale   *ScaleAction      `json:"scale,omitempty"`
	Notify  string            `json:"notify,omitempty"`
	Retries int               `json:"retries,omitempty"`
	Inputs  []string          `json:"inputs,omitempty"`  // "step/name"
	Outputs []string          `json:"outputs,omitempty"` // "name"
	Kind    string            `json:"kind"`              // derived: app|image|deploy|scale|notify
}

// Spec is a compiled, validated pipeline.yaml. Steps is topologically
// sorted (Kahn's algorithm, ties broken by name ascending), so compiling
// the same YAML twice always yields the same order — the order
// CreatePipelineRun uses to pre-expand pipeline_run_steps rows.
type Spec struct {
	Steps []Step `json:"steps"`
}

// rawSpec/rawStep mirror pipeline.yaml's shape for unmarshaling via
// sigs.k8s.io/yaml (YAML -> JSON -> encoding/json under the hood, hence the
// json tags rather than yaml tags).
type rawSpec struct {
	Steps map[string]rawStep `json:"steps"`
}

type rawStep struct {
	Needs   []string          `json:"needs"`
	App     string            `json:"app"`
	Image   string            `json:"image"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
	GPU     int               `json:"gpu"`
	Deploy  string            `json:"deploy"`
	Scale   *ScaleAction      `json:"scale"`
	Notify  string            `json:"notify"`
	Retries int               `json:"retries"`
	Inputs  []string          `json:"inputs"`
	Outputs []string          `json:"outputs"`
}

// Compile parses and validates pipeline.yaml. Structural validation only —
// app existence/kind checks are the server's job (it has the store).
func Compile(b []byte) (Spec, error) {
	if len(b) > maxYAMLBytes {
		return Spec{}, fmt.Errorf("pipeline.yaml exceeds %d byte limit", maxYAMLBytes)
	}

	var raw rawSpec
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return Spec{}, fmt.Errorf("parse pipeline.yaml: %w", err)
	}
	if len(raw.Steps) == 0 {
		return Spec{}, fmt.Errorf("pipeline.yaml: top level must be a non-empty steps map")
	}

	names := make([]string, 0, len(raw.Steps))
	for name := range raw.Steps {
		names = append(names, name)
	}
	sort.Strings(names)

	steps := make(map[string]Step, len(raw.Steps))
	for _, name := range names {
		rs := raw.Steps[name]

		if !stepNameRe.MatchString(name) || len(name) > 20 {
			return Spec{}, fmt.Errorf("step %q: invalid name (must match %s, max 20 chars)", name, stepNameRe.String())
		}

		kind, err := deriveKind(name, rs)
		if err != nil {
			return Spec{}, err
		}
		if err := checkFieldLegality(name, kind, rs); err != nil {
			return Spec{}, err
		}
		if err := checkStepValues(name, kind, rs); err != nil {
			return Spec{}, err
		}

		steps[name] = Step{
			Name:    name,
			Needs:   rs.Needs,
			App:     rs.App,
			Image:   rs.Image,
			Command: rs.Command,
			Env:     rs.Env,
			GPU:     rs.GPU,
			Deploy:  rs.Deploy,
			Scale:   rs.Scale,
			Notify:  rs.Notify,
			Retries: rs.Retries,
			Inputs:  rs.Inputs,
			Outputs: rs.Outputs,
			Kind:    kind,
		}
	}

	// needs: dangling references and self-need.
	for _, name := range names {
		for _, dep := range steps[name].Needs {
			if dep == name {
				return Spec{}, fmt.Errorf("step %q: cannot need itself", name)
			}
			if _, ok := steps[dep]; !ok {
				return Spec{}, fmt.Errorf("step %q: needs unknown step %q", name, dep)
			}
		}
	}

	if cyc := findCycle(names, steps); cyc != "" {
		return Spec{}, fmt.Errorf("pipeline.yaml: cycle detected involving step %q", cyc)
	}

	// inputs must reference a declared output of a TRANSITIVE upstream step.
	for _, name := range names {
		st := steps[name]
		upstream := transitiveNeeds(name, steps)
		for _, in := range st.Inputs {
			parts := strings.SplitN(in, "/", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return Spec{}, fmt.Errorf("step %q: input %q must be \"step/name\"", name, in)
			}
			srcStep, outName := parts[0], parts[1]
			src, ok := steps[srcStep]
			if !ok {
				return Spec{}, fmt.Errorf("step %q: input %q references unknown step %q", name, in, srcStep)
			}
			if !upstream[srcStep] {
				return Spec{}, fmt.Errorf("step %q: input %q does not reference a transitive upstream step", name, in)
			}
			if !containsStr(src.Outputs, outName) {
				return Spec{}, fmt.Errorf("step %q: input %q references undeclared output %q of step %q", name, in, outName, srcStep)
			}
		}
	}

	sorted, err := topoSort(names, steps)
	if err != nil {
		return Spec{}, err
	}
	return Spec{Steps: sorted}, nil
}

// deriveKind determines which of app|image|deploy|scale|notify is set on a
// raw step — exactly one must be.
func deriveKind(name string, rs rawStep) (string, error) {
	set := map[string]bool{}
	if rs.App != "" {
		set["app"] = true
	}
	if rs.Image != "" {
		set["image"] = true
	}
	if rs.Deploy != "" {
		set["deploy"] = true
	}
	if rs.Scale != nil {
		set["scale"] = true
	}
	if rs.Notify != "" {
		set["notify"] = true
	}
	if len(set) != 1 {
		kinds := make([]string, 0, len(set))
		for k := range set {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		return "", fmt.Errorf("step %q: exactly one of app|image|deploy|scale|notify required, got %v", name, kinds)
	}
	for k := range set {
		return k, nil
	}
	panic("unreachable") // len(set) == 1 guarantees a range iteration
}

// checkFieldLegality rejects fields that don't apply to the step's derived
// kind: command/gpu are image-only; env/retries/inputs/outputs are
// app-or-image-only.
func checkFieldLegality(name, kind string, rs rawStep) error {
	if kind != "image" {
		if len(rs.Command) > 0 {
			return fmt.Errorf("step %q: command only allowed on image steps", name)
		}
		if rs.GPU != 0 {
			return fmt.Errorf("step %q: gpu only allowed on image steps", name)
		}
	}
	if kind != "app" && kind != "image" {
		if len(rs.Env) > 0 {
			return fmt.Errorf("step %q: env only allowed on app or image steps", name)
		}
		if rs.Retries != 0 {
			return fmt.Errorf("step %q: retries only allowed on app or image steps", name)
		}
		if len(rs.Inputs) > 0 {
			return fmt.Errorf("step %q: inputs only allowed on app or image steps", name)
		}
		if len(rs.Outputs) > 0 {
			return fmt.Errorf("step %q: outputs only allowed on app or image steps", name)
		}
	}
	return nil
}

// checkStepValues validates value ranges/formats: scale.replicas, notify
// message length, env keys, and output name legality/uniqueness.
func checkStepValues(name, kind string, rs rawStep) error {
	switch kind {
	case "scale":
		if rs.Scale.Replicas < 0 || rs.Scale.Replicas > 50 {
			return fmt.Errorf("step %q: scale.replicas must be 0..50, got %d", name, rs.Scale.Replicas)
		}
	case "notify":
		if len(rs.Notify) == 0 {
			return fmt.Errorf("step %q: notify message must not be empty", name)
		}
		if len(rs.Notify) > 500 {
			return fmt.Errorf("step %q: notify message exceeds 500 chars", name)
		}
	}

	for k := range rs.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("step %q: invalid env key %q (must match %s)", name, k, envKeyRe.String())
		}
	}

	seenOutputs := make(map[string]bool, len(rs.Outputs))
	for _, o := range rs.Outputs {
		if !envKeyRe.MatchString(o) {
			return fmt.Errorf("step %q: invalid output name %q (must match %s)", name, o, envKeyRe.String())
		}
		if seenOutputs[o] {
			return fmt.Errorf("step %q: duplicate output %q", name, o)
		}
		seenOutputs[o] = true
	}
	return nil
}

// findCycle runs a three-color DFS over the Needs graph and returns the
// name of a step involved in a cycle, or "" if the graph is a DAG.
func findCycle(names []string, steps map[string]Step) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(steps))
	var cycleStep string

	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = gray
		for _, dep := range steps[n].Needs {
			switch color[dep] {
			case gray:
				cycleStep = dep
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		color[n] = black
		return false
	}

	for _, n := range names {
		if color[n] == white {
			if visit(n) {
				return cycleStep
			}
		}
	}
	return ""
}

// transitiveNeeds returns the set of every step name transitively required
// by name via Needs edges (the upstream closure). Requires an acyclic
// graph — callers run this only after findCycle has passed.
func transitiveNeeds(name string, steps map[string]Step) map[string]bool {
	seen := map[string]bool{}
	var walk func(n string)
	walk = func(n string) {
		for _, dep := range steps[n].Needs {
			if !seen[dep] {
				seen[dep] = true
				walk(dep)
			}
		}
	}
	walk(name)
	return seen
}

// topoSort orders steps via Kahn's algorithm; the ready set is sorted by
// name ascending before each pop, making the result deterministic across
// compiles of the same YAML regardless of Go's map iteration order.
func topoSort(names []string, steps map[string]Step) ([]Step, error) {
	indegree := make(map[string]int, len(names))
	dependents := make(map[string][]string, len(names))
	for _, n := range names {
		indegree[n] = len(steps[n].Needs)
	}
	for _, n := range names {
		for _, dep := range steps[n].Needs {
			dependents[dep] = append(dependents[dep], n)
		}
	}

	var ready []string
	for _, n := range names {
		if indegree[n] == 0 {
			ready = append(ready, n)
		}
	}

	var out []Step
	for len(ready) > 0 {
		sort.Strings(ready)
		n := ready[0]
		ready = ready[1:]
		out = append(out, steps[n])

		next := dependents[n]
		sort.Strings(next)
		for _, m := range next {
			indegree[m]--
			if indegree[m] == 0 {
				ready = append(ready, m)
			}
		}
	}

	if len(out) != len(names) {
		// Unreachable in practice: Compile runs findCycle before topoSort.
		// Kept as a safety net against future refactors that reorder calls.
		return nil, fmt.Errorf("pipeline.yaml: cycle detected during topological sort")
	}
	return out, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Step returns the named step and whether it exists.
func (sp Spec) Step(name string) (Step, bool) {
	for _, st := range sp.Steps {
		if st.Name == name {
			return st, true
		}
	}
	return Step{}, false
}

// Downstream returns the transitive dependents of step name (fail-fast
// skip set): every step whose Needs closure includes name, sorted by name.
func (sp Spec) Downstream(name string) []string {
	byName := make(map[string]Step, len(sp.Steps))
	for _, st := range sp.Steps {
		byName[st.Name] = st
	}
	var out []string
	for _, st := range sp.Steps {
		if st.Name == name {
			continue
		}
		if dependsOn(st.Name, name, byName, map[string]bool{}) {
			out = append(out, st.Name)
		}
	}
	sort.Strings(out)
	return out
}

func dependsOn(from, target string, byName map[string]Step, seen map[string]bool) bool {
	if seen[from] {
		return false
	}
	seen[from] = true
	for _, dep := range byName[from].Needs {
		if dep == target {
			return true
		}
		if dependsOn(dep, target, byName, seen) {
			return true
		}
	}
	return false
}

// ArtifactEnv returns the artifact + convention env for one step:
// LUNCUR_PIPELINE_ID, LUNCUR_PIPELINE_RUN_ID, LUNCUR_ARTIFACT_PREFIX
// (= "pipelines/<pipelineName>/<runID>/"), LUNCUR_OUTPUT_<NAME> and
// LUNCUR_INPUT_<NAME> per declaration. Step.Env is NOT merged here — the
// caller overlays it (step env wins over these convention values).
func ArtifactEnv(pipelineName, runID string, st Step) map[string]string {
	prefix := fmt.Sprintf("pipelines/%s/%s/", pipelineName, runID)
	env := map[string]string{
		"LUNCUR_PIPELINE_ID":     pipelineName,
		"LUNCUR_PIPELINE_RUN_ID": runID,
		"LUNCUR_ARTIFACT_PREFIX": prefix,
	}
	for _, out := range st.Outputs {
		env["LUNCUR_OUTPUT_"+strings.ToUpper(out)] = prefix + st.Name + "/" + out
	}
	for _, in := range st.Inputs {
		parts := strings.SplitN(in, "/", 2)
		if len(parts) != 2 {
			continue
		}
		srcStep, name := parts[0], parts[1]
		env["LUNCUR_INPUT_"+strings.ToUpper(name)] = prefix + srcStep + "/" + name
	}
	return env
}
