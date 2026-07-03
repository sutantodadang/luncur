package render

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestOverrideMergesIntoDeployment(t *testing.T) {
	in := testInput()
	in.Overrides = map[string]string{
		"Deployment": `{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"256Mi"}}}]}}}}`,
	}
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	// Strategic merge: patch by container name merges INTO the container,
	// preserving image/ports while adding resources.
	if c.Image != in.Image {
		t.Fatalf("image lost in merge: %q", c.Image)
	}
	if got := c.Resources.Limits.Memory().String(); got != "256Mi" {
		t.Fatalf("memory limit: %s", got)
	}
}

func TestOverrideBaseRenderStillWinsElsewhere(t *testing.T) {
	// An override set when replicas was 2 must not pin replicas after the
	// model changes — only fields the patch names are overridden.
	in := testInput()
	in.Overrides = map[string]string{"Deployment": `{"metadata":{"labels":{"team":"x"}}}`}
	in.Replicas = 5
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if *d.Spec.Replicas != 5 {
		t.Fatalf("replicas: %d", *d.Spec.Replicas)
	}
	if d.Labels["team"] != "x" || d.Labels["app.kubernetes.io/managed-by"] != "luncur" {
		t.Fatalf("labels: %v", d.Labels)
	}
}

func TestOverrideUnknownKindErrors(t *testing.T) {
	in := testInput()
	in.Overrides = map[string]string{"Secret": `{}`}
	if _, err := Render(in, map[string]string{"A": "1"}); err == nil {
		t.Fatal("want error for Secret override")
	}
}

func TestOverrideInvalidPatchErrors(t *testing.T) {
	in := testInput()
	in.Overrides = map[string]string{"Deployment": `{"spec":{"replicas":"not-a-number"}}`}
	if _, err := Render(in, nil); err == nil {
		t.Fatal("want error for type-mismatched patch")
	}
}
