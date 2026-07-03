package kube

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/render"
)

// newFakeDyn builds a fake dynamic client seeded with objs, following the
// same empty-scheme construction the rest of this file uses (see
// fakeClient / TestDeleteAppObjectsIgnoresNotFound). Unstructured objects
// carry their own GVK, and the fake dynamic client infers the plural
// resource name from Kind, so no extra scheme registration is needed for
// regularly-pluralized kinds like Deployment.
func newFakeDyn(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClient(scheme, objs...)
}

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

// TestApplyClusterRoleBindingSkipsNamespace guards against Apply treating
// ClusterRoleBinding (cluster-scoped, per gvrByKind) as namespaced. Using
// the recorded-action fakeClient is the simplest reliable path: the fake
// dynamic client's short-circuit reactor never round-trips through a real
// ObjectTracker, so asserting on the recorded namespace is sufficient
// without needing a Get-based round trip.
func TestApplyClusterRoleBindingSkipsNamespace(t *testing.T) {
	c, log := fakeClient(t)
	crb := []render.Object{{
		Kind: "ClusterRoleBinding",
		JSON: json.RawMessage(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"luncur-crb"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"cluster-admin"},"subjects":[{"kind":"ServiceAccount","name":"luncur","namespace":"luncur-system"}]}`),
	}}
	if err := c.Apply(context.Background(), "luncur-web", crb); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(*log), *log)
	}
	rec := (*log)[0]
	if rec.resource != "clusterrolebindings" || rec.name != "luncur-crb" {
		t.Fatalf("bad action: %+v", rec)
	}
	if rec.namespace != "" {
		t.Fatalf("want no namespace on cluster-scoped patch, got %q", rec.namespace)
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

func TestWaitJobSucceeded(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("get", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": "build-1", "namespace": "luncur-system"},
			"status":   map[string]any{"succeeded": int64(1)},
		}}
		return true, u, nil
	})
	c := NewFromDynamic(dyn)
	ok, err := c.WaitJob(context.Background(), "luncur-system", "build-1", time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("WaitJob = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestAppPodsAndNodeIP(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: "proj",
			Labels: map[string]string{"app.kubernetes.io/name": "web"},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "other-1", Namespace: "proj",
			Labels: map[string]string{"app.kubernetes.io/name": "other"},
		}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.9"},
			}},
		},
	)
	c := NewForTest(nil, cs)

	pods, err := c.AppPods(context.Background(), "proj", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0] != "web-1" {
		t.Fatalf("pods = %v, want [web-1]", pods)
	}

	ip, err := c.NodeIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ip != "203.0.113.9" {
		t.Fatalf("ip = %q, want ExternalIP preferred", ip)
	}
}

func TestWaitDeployment(t *testing.T) {
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "luncur", "namespace": "luncur-system"},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	dyn := newFakeDyn(t, dep)
	c := NewForTest(dyn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitDeployment(ctx, "luncur-system", "luncur", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestHasGroupVersion(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{
		{GroupVersion: "helm.cattle.io/v1", APIResources: []metav1.APIResource{{Name: "helmchartconfigs"}}},
	}
	c := NewForTest(nil, cs)
	ok, err := c.HasGroupVersion(context.Background(), "helm.cattle.io/v1")
	if err != nil || !ok {
		t.Fatalf("helm gv: ok=%v err=%v", ok, err)
	}
	ok, err = c.HasGroupVersion(context.Background(), "cert-manager.io/v1")
	if err != nil || ok {
		t.Fatalf("absent gv: ok=%v err=%v, want false nil", ok, err)
	}
}

// TestApplyClusterIssuerIsClusterScoped mirrors
// TestApplyClusterRoleBindingSkipsNamespace: ClusterIssuer (cluster-scoped,
// per gvrByKind) must also be patched without a namespace.
func TestApplyClusterIssuerIsClusterScoped(t *testing.T) {
	c, log := fakeClient(t)
	ci := []render.Object{{
		Kind: "ClusterIssuer",
		JSON: json.RawMessage(`{"apiVersion":"cert-manager.io/v1","kind":"ClusterIssuer","metadata":{"name":"luncur-le"}}`),
	}}
	if err := c.Apply(context.Background(), "luncur-web", ci); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(*log), *log)
	}
	rec := (*log)[0]
	if rec.resource != "clusterissuers" || rec.name != "luncur-le" {
		t.Fatalf("bad action: %+v", rec)
	}
	if rec.namespace != "" {
		t.Fatalf("want no namespace on cluster-scoped patch, got %q", rec.namespace)
	}
}
