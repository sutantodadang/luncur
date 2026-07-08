package pipeline

import (
	"strings"
	"testing"
)

const exampleYAML = `
steps:
  prepare:
    app: prep-job
    env: {MODE: full}
    outputs: [dataset]

  train:
    needs: [prepare]
    app: train-job
    inputs: [prepare/dataset]
    retries: 1
    outputs: [model]

  evaluate:
    needs: [train]
    image: ghcr.io/you/eval:v1
    command: ["python", "eval.py"]
    env: {THRESH: "0.9"}
    gpu: 0
    inputs: [train/model]

  publish:
    needs: [evaluate]
    deploy: chat

  serve-up:
    needs: [publish]
    scale: {app: chat, replicas: 1}

  done:
    needs: [serve-up]
    notify: "model baru live"
`

func TestCompileHappyPath(t *testing.T) {
	sp, err := Compile([]byte(exampleYAML))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(sp.Steps) != 6 {
		t.Fatalf("want 6 steps, got %d", len(sp.Steps))
	}
	// topo order: prepare must precede train, train precede evaluate, etc.
	order := make(map[string]int, len(sp.Steps))
	for i, st := range sp.Steps {
		order[st.Name] = i
	}
	if order["prepare"] > order["train"] || order["train"] > order["evaluate"] ||
		order["evaluate"] > order["publish"] || order["publish"] > order["serve-up"] ||
		order["serve-up"] > order["done"] {
		t.Fatalf("topo order violated: %+v", order)
	}

	prep, ok := sp.Step("prepare")
	if !ok || prep.Kind != "app" || prep.App != "prep-job" {
		t.Fatalf("prepare step: %+v ok=%v", prep, ok)
	}
	train, ok := sp.Step("train")
	if !ok || train.Kind != "app" || len(train.Needs) != 1 || train.Needs[0] != "prepare" {
		t.Fatalf("train step: %+v ok=%v", train, ok)
	}
	eval, ok := sp.Step("evaluate")
	if !ok || eval.Kind != "image" || eval.Image != "ghcr.io/you/eval:v1" {
		t.Fatalf("evaluate step: %+v ok=%v", eval, ok)
	}
	pub, ok := sp.Step("publish")
	if !ok || pub.Kind != "deploy" || pub.Deploy != "chat" {
		t.Fatalf("publish step: %+v ok=%v", pub, ok)
	}
	su, ok := sp.Step("serve-up")
	if !ok || su.Kind != "scale" || su.Scale == nil || su.Scale.App != "chat" || su.Scale.Replicas != 1 {
		t.Fatalf("serve-up step: %+v ok=%v", su, ok)
	}
	done, ok := sp.Step("done")
	if !ok || done.Kind != "notify" || done.Notify != "model baru live" {
		t.Fatalf("done step: %+v ok=%v", done, ok)
	}
}

func TestCompileTopoOrderDeterministic(t *testing.T) {
	sp1, err := Compile([]byte(exampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	sp2, err := Compile([]byte(exampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	for i := range sp1.Steps {
		if sp1.Steps[i].Name != sp2.Steps[i].Name {
			t.Fatalf("nondeterministic topo order: %v vs %v", namesOf(sp1), namesOf(sp2))
		}
	}
}

func namesOf(sp Spec) []string {
	out := make([]string, len(sp.Steps))
	for i, st := range sp.Steps {
		out[i] = st.Name
	}
	return out
}

func TestCompileDiamondDAGOrder(t *testing.T) {
	// b needs a; c needs a; d needs b,c (diamond join). Root "a" then b,c
	// (both roots after a resolves, name-asc tiebreak: b before c), then d.
	yamlSrc := `
steps:
  a:
    app: a-job
  b:
    needs: [a]
    app: b-job
  c:
    needs: [a]
    app: c-job
  d:
    needs: [b, c]
    app: d-job
`
	sp, err := Compile([]byte(yamlSrc))
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(sp)
	want := []string{"a", "b", "c", "d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("diamond order = %v, want %v", got, want)
	}
}

func TestCompileKindDerivation(t *testing.T) {
	cases := []struct {
		name, yamlSrc, wantKind string
	}{
		{"app", "steps:\n  s:\n    app: x\n", "app"},
		{"image", "steps:\n  s:\n    image: x:latest\n", "image"},
		{"deploy", "steps:\n  s:\n    deploy: x\n", "deploy"},
		{"scale", "steps:\n  s:\n    scale: {app: x, replicas: 2}\n", "scale"},
		{"notify", "steps:\n  s:\n    notify: hi\n", "notify"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp, err := Compile([]byte(c.yamlSrc))
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			st, ok := sp.Step("s")
			if !ok || st.Kind != c.wantKind {
				t.Fatalf("kind = %+v, want %q", st, c.wantKind)
			}
		})
	}
}

func mustErr(t *testing.T, yamlSrc, why string) {
	t.Helper()
	if _, err := Compile([]byte(yamlSrc)); err == nil {
		t.Fatalf("%s: want error, got nil", why)
	}
}

func TestCompileErrors(t *testing.T) {
	mustErr(t, "steps:\n  s:\n    app: x\n    image: y\n", "two kinds on one step")
	mustErr(t, "steps:\n  s:\n    needs: []\n", "zero kinds")
	mustErr(t, "steps:\n  a:\n    needs: [b]\n    app: x\n  b:\n    needs: [a]\n    app: y\n", "cycle a->b->a")
	mustErr(t, "steps:\n  a:\n    needs: [nope]\n    app: x\n", "dangling needs")
	mustErr(t, "steps:\n  a:\n    needs: [a]\n    app: x\n", "self-need")
	mustErr(t, `steps:
  a:
    app: x
    outputs: [dataset]
  b:
    app: y
  c:
    needs: [b]
    app: z
    inputs: [a/dataset]
`, "input referencing non-upstream step")
	mustErr(t, `steps:
  a:
    app: x
    outputs: [dataset]
  b:
    needs: [a]
    app: y
    inputs: [a/nope]
`, "input naming undeclared output")
	mustErr(t, "steps:\n  Bad_Name:\n    app: x\n", "bad step name")
	mustErr(t, "steps:\n  this-name-is-way-too-long-abc:\n    app: x\n", ">20-char name")
	mustErr(t, "steps:\n  a:\n    app: x\n    env: {\"bad key\": v}\n", "bad env key")
	mustErr(t, "steps:\n  a:\n    app: x\n    command: [\"echo\"]\n", "command on app step")
	mustErr(t, "steps:\n  a:\n    deploy: x\n    retries: 2\n", "retries on action")

	oversized := "steps:\n  a:\n    app: x\n    env: {K: \"" + strings.Repeat("x", 70*1024) + "\"}\n"
	mustErr(t, oversized, "oversized YAML")
}

func TestArtifactEnv(t *testing.T) {
	sp, err := Compile([]byte(exampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	train, ok := sp.Step("train")
	if !ok {
		t.Fatal("train step missing")
	}
	env := ArtifactEnv("pl", "run1", train)
	if env["LUNCUR_PIPELINE_ID"] != "pl" {
		t.Fatalf("LUNCUR_PIPELINE_ID = %q", env["LUNCUR_PIPELINE_ID"])
	}
	if env["LUNCUR_PIPELINE_RUN_ID"] != "run1" {
		t.Fatalf("LUNCUR_PIPELINE_RUN_ID = %q", env["LUNCUR_PIPELINE_RUN_ID"])
	}
	if env["LUNCUR_ARTIFACT_PREFIX"] != "pipelines/pl/run1/" {
		t.Fatalf("LUNCUR_ARTIFACT_PREFIX = %q", env["LUNCUR_ARTIFACT_PREFIX"])
	}
	if env["LUNCUR_OUTPUT_MODEL"] != "pipelines/pl/run1/train/model" {
		t.Fatalf("LUNCUR_OUTPUT_MODEL = %q", env["LUNCUR_OUTPUT_MODEL"])
	}
	if env["LUNCUR_INPUT_DATASET"] != "pipelines/pl/run1/prepare/dataset" {
		t.Fatalf("LUNCUR_INPUT_DATASET = %q", env["LUNCUR_INPUT_DATASET"])
	}

	// convention trio always present even with no inputs/outputs declared.
	deploy, ok := sp.Step("publish")
	if !ok {
		t.Fatal("publish step missing")
	}
	env2 := ArtifactEnv("pl", "run1", deploy)
	for _, k := range []string{"LUNCUR_PIPELINE_ID", "LUNCUR_PIPELINE_RUN_ID", "LUNCUR_ARTIFACT_PREFIX"} {
		if _, ok := env2[k]; !ok {
			t.Fatalf("convention env %q missing: %+v", k, env2)
		}
	}
}

func TestDownstream(t *testing.T) {
	sp, err := Compile([]byte(exampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	down := sp.Downstream("train")
	want := []string{"done", "evaluate", "publish", "serve-up"}
	if strings.Join(down, ",") != strings.Join(want, ",") {
		t.Fatalf("Downstream(train) = %v, want %v", down, want)
	}
	if len(sp.Downstream("done")) != 0 {
		t.Fatalf("Downstream(done) should be empty (leaf), got %v", sp.Downstream("done"))
	}
}
