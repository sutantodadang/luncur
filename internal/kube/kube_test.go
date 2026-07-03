package kube

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/sutantodadang/luncur/internal/render"
)

type recorded struct {
	verb      string
	resource  string
	namespace string
	name      string
	patchType string
	patch     []byte
}

// fakeClient returns a Client whose dynamic layer records every action.
func fakeClient(t *testing.T) (*Client, *[]recorded) {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var log []recorded
	dyn.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		rec := recorded{
			verb:      action.GetVerb(),
			resource:  action.GetResource().Resource,
			namespace: action.GetNamespace(),
		}
		switch a := action.(type) {
		case ktesting.PatchAction:
			rec.name = a.GetName()
			rec.patchType = string(a.GetPatchType())
			rec.patch = a.GetPatch()
		case ktesting.DeleteAction:
			rec.name = a.GetName()
		}
		log = append(log, rec)
		return true, nil, nil // short-circuit: we assert on actions, not state
	})
	return NewFromDynamic(dyn), &log
}

func renderedObjects(t *testing.T) []render.Object {
	t.Helper()
	r, err := render.Render(render.Input{
		AppName: "api", Namespace: "luncur-web",
		Image: "nginx", Host: "api.1-2-3-4.sslip.io", Port: 3000, Replicas: 1,
	}, map[string]string{"K": "v"})
	if err != nil {
		t.Fatal(err)
	}
	return r.Objects
}

func TestApplyUsesSSAForEveryObject(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.Apply(context.Background(), "luncur-web", renderedObjects(t)); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 4 {
		t.Fatalf("want 4 actions, got %d: %+v", len(*log), *log)
	}
	wantResources := []string{"secrets", "deployments", "services", "ingresses"}
	for i, rec := range *log {
		if rec.verb != "patch" || rec.patchType != "application/apply-patch+yaml" {
			t.Errorf("action %d: want SSA patch, got %+v", i, rec)
		}
		if rec.resource != wantResources[i] {
			t.Errorf("action %d: want %s, got %s", i, wantResources[i], rec.resource)
		}
		if rec.namespace != "luncur-web" {
			t.Errorf("action %d: namespace %s", i, rec.namespace)
		}
	}
	if (*log)[0].name != "api-env" || (*log)[1].name != "api" {
		t.Errorf("names: %+v", *log)
	}
}

func TestEnsureNamespace(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.EnsureNamespace(context.Background(), "luncur-web"); err != nil {
		t.Fatal(err)
	}
	rec := (*log)[0]
	if rec.verb != "patch" || rec.resource != "namespaces" || rec.name != "luncur-web" {
		t.Fatalf("bad action: %+v", rec)
	}
	var body struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(rec.patch, &body); err != nil {
		t.Fatalf("unmarshal patch body: %v", err)
	}
	if body.Metadata.Labels["app.kubernetes.io/managed-by"] != "luncur" {
		t.Fatalf("managed-by label missing: %+v", body.Metadata.Labels)
	}
	if body.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("pod-security enforce label missing: %+v", body.Metadata.Labels)
	}
}

func TestDeleteAppObjectsIgnoresNotFound(t *testing.T) {
	// Default reactor chain (no short-circuit): deleting non-existent
	// objects from the empty fake tracker returns NotFound, which
	// DeleteAppObjects must swallow.
	scheme := runtime.NewScheme()
	c := NewFromDynamic(dynamicfake.NewSimpleDynamicClient(scheme))
	if err := c.DeleteAppObjects(context.Background(), "luncur-web", "api"); err != nil {
		t.Fatalf("NotFound should be ignored: %v", err)
	}
}

func TestApplyRejectsObjectWithoutName(t *testing.T) {
	c, _ := fakeClient(t)
	bad := []render.Object{{Kind: "Service", JSON: json.RawMessage(`{"metadata":{}}`)}}
	if err := c.Apply(context.Background(), "ns", bad); err == nil {
		t.Fatal("want error for object without metadata.name")
	}
}
