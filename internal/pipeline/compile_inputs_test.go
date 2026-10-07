package pipeline

import (
	"strings"
	"testing"
)

// Two inputs sharing an output name (train-a/model, train-b/model) would both
// map to LUNCUR_INPUT_MODEL, silently dropping one; Compile rejects it.
func TestInputsWithSameOutputNameDontCollide(t *testing.T) {
	spec, err := Compile([]byte(`
steps:
  train-a:
    image: x
    outputs: [model]
  train-b:
    image: x
    outputs: [model]
  ensemble:
    image: x
    needs: [train-a, train-b]
    inputs: [train-a/model, train-b/model]
`))
	if err == nil {
		st, _ := spec.Step("ensemble")
		t.Fatalf("Compile accepted colliding inputs; env = %v", ArtifactEnv("p", "r", st))
	}
	if !strings.Contains(err.Error(), "LUNCUR_INPUT_MODEL") {
		t.Fatalf("error = %v, want it to name the colliding env var", err)
	}
}
