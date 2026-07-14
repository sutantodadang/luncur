package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	if body.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatalf("pod-security enforce label missing: %+v", body.Metadata.Labels)
	}
}

func TestEnsureNamespaceWithPolicyBaseline(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.EnsureNamespaceWithPolicy(context.Background(), "luncur-system", "baseline"); err != nil {
		t.Fatal(err)
	}
	rec := (*log)[0]
	if rec.verb != "patch" || rec.resource != "namespaces" || rec.name != "luncur-system" {
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
	if body.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatalf("pod-security enforce label want baseline: %+v", body.Metadata.Labels)
	}
}

func TestApplyIsolation(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.ApplyIsolation(context.Background(), "luncur-web"); err != nil {
		t.Fatal(err)
	}
	rec := (*log)[0]
	if rec.verb != "patch" || rec.resource != "networkpolicies" || rec.namespace != "luncur-web" || rec.name != "luncur-isolation" {
		t.Fatalf("bad action: %+v", rec)
	}
	if rec.patchType != "application/apply-patch+yaml" {
		t.Fatalf("want SSA patch, got %q", rec.patchType)
	}
	var body struct {
		Spec struct {
			PolicyTypes []string `json:"policyTypes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(rec.patch, &body); err != nil {
		t.Fatalf("unmarshal patch body: %v", err)
	}
	if len(body.Spec.PolicyTypes) != 1 || body.Spec.PolicyTypes[0] != "Ingress" {
		t.Fatalf("want Ingress-only policyTypes, got %+v", body.Spec.PolicyTypes)
	}
}

func TestRemoveIsolationIgnoresNotFound(t *testing.T) {
	// Same empty-tracker shape as TestDeleteAppObjectsIgnoresNotFound: no
	// NetworkPolicy was ever applied, so the delete hits NotFound, which
	// RemoveIsolation (via DeleteObject) must swallow.
	scheme := runtime.NewScheme()
	c := NewFromDynamic(dynamicfake.NewSimpleDynamicClient(scheme))
	if err := c.RemoveIsolation(context.Background(), "luncur-web"); err != nil {
		t.Fatalf("NotFound should be ignored: %v", err)
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

func TestJobExists(t *testing.T) {
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "build-1", "namespace": "luncur-system"},
	}}
	dyn := newFakeDyn(t, job)
	c := NewForTest(dyn, nil)

	ok, err := c.JobExists(context.Background(), "luncur-system", "build-1")
	if err != nil || !ok {
		t.Fatalf("JobExists(build-1) = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = c.JobExists(context.Background(), "luncur-system", "build-absent")
	if err != nil || ok {
		t.Fatalf("JobExists(build-absent) = (%v, %v), want (false, nil)", ok, err)
	}
}

func jobWithStatus(name string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": "luncur-system"},
		"status":    status,
	}}
}

func TestJobDoneSucceeded(t *testing.T) {
	dyn := newFakeDyn(t, jobWithStatus("pl-run1-train-a1", map[string]any{"succeeded": int64(1)}))
	c := NewForTest(dyn, nil)

	done, failed, err := c.JobDone(context.Background(), "luncur-system", "pl-run1-train-a1")
	if err != nil || !done || failed {
		t.Fatalf("JobDone(succeeded) = (%v, %v, %v), want (true, false, nil)", done, failed, err)
	}
}

func TestJobDoneFailed(t *testing.T) {
	dyn := newFakeDyn(t, jobWithStatus("pl-run1-train-a1", map[string]any{"failed": int64(1)}))
	c := NewForTest(dyn, nil)

	done, failed, err := c.JobDone(context.Background(), "luncur-system", "pl-run1-train-a1")
	if err != nil || !done || !failed {
		t.Fatalf("JobDone(failed) = (%v, %v, %v), want (true, true, nil)", done, failed, err)
	}
}

func TestJobDoneStillRunning(t *testing.T) {
	dyn := newFakeDyn(t, jobWithStatus("pl-run1-train-a1", map[string]any{"active": int64(1)}))
	c := NewForTest(dyn, nil)

	done, failed, err := c.JobDone(context.Background(), "luncur-system", "pl-run1-train-a1")
	if err != nil || done || failed {
		t.Fatalf("JobDone(running) = (%v, %v, %v), want (false, false, nil)", done, failed, err)
	}
}

func TestJobDoneNotFound(t *testing.T) {
	dyn := newFakeDyn(t)
	c := NewForTest(dyn, nil)

	done, failed, err := c.JobDone(context.Background(), "luncur-system", "does-not-exist")
	if err != nil || done || failed {
		t.Fatalf("JobDone(missing) = (%v, %v, %v), want (false, false, nil)", done, failed, err)
	}
}

func TestJobPodStatusPending(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "build-1-abcde", Namespace: "luncur-system",
			Labels: map[string]string{"job-name": "build-1"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
	cs := k8sfake.NewSimpleClientset(pod)
	c := NewForTest(nil, cs)

	phase, reason, err := c.JobPodStatus(context.Background(), "luncur-system", "build-1")
	if err != nil {
		t.Fatal(err)
	}
	if phase != "Pending" || reason != "ImagePullBackOff" {
		t.Fatalf("JobPodStatus = (%q, %q), want (Pending, ImagePullBackOff)", phase, reason)
	}
}

func TestJobPodStatusRunningNoPods(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "build-2-fghij", Namespace: "luncur-system",
			Labels: map[string]string{"job-name": "build-2"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := k8sfake.NewSimpleClientset(pod)
	c := NewForTest(nil, cs)

	phase, reason, err := c.JobPodStatus(context.Background(), "luncur-system", "build-2")
	if err != nil {
		t.Fatal(err)
	}
	if phase != "Running" || reason != "" {
		t.Fatalf("JobPodStatus = (%q, %q), want (Running, \"\")", phase, reason)
	}

	phase, reason, err = c.JobPodStatus(context.Background(), "luncur-system", "build-absent")
	if err != nil {
		t.Fatal(err)
	}
	if phase != "" || reason != "" {
		t.Fatalf("JobPodStatus(no pods) = (%q, %q), want (\"\", \"\")", phase, reason)
	}
}

// TestJobEvents seeds two Job events (an older FailedCreate from a
// PodSecurity rejection and a newer BackoffLimitExceeded) and checks
// JobEvents formats both, oldest first. Note: the fake clientset's generic
// List (see gentype.alsoFakeLister.List) only applies the label selector
// client-side and discards the field selector, so this doesn't exercise
// JobEvents' involvedObject FieldSelector filtering against a real API
// server — it only checks formatting/ordering. The empty case below (no
// events seeded at all) covers the no-events path without depending on
// field-selector filtering.
func TestJobEvents(t *testing.T) {
	older := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	newer := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	e1 := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "build-1.1", Namespace: "luncur-system"},
		InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: "build-1"},
		Type:           "Warning",
		Reason:         "FailedCreate",
		Message:        `pods "build-1-" is forbidden: violates PodSecurity "restricted:latest"`,
		LastTimestamp:  older,
	}
	e2 := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "build-1.2", Namespace: "luncur-system"},
		InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: "build-1"},
		Type:           "Warning",
		Reason:         "BackoffLimitExceeded",
		Message:        "Job has reached the specified backoff limit",
		LastTimestamp:  newer,
	}
	cs := k8sfake.NewSimpleClientset(e1, e2)
	c := NewForTest(nil, cs)

	got, err := c.JobEvents(context.Background(), "luncur-system", "build-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`Warning FailedCreate: pods "build-1-" is forbidden: violates PodSecurity "restricted:latest"`,
		"Warning BackoffLimitExceeded: Job has reached the specified backoff limit",
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("JobEvents = %v, want %v", got, want)
	}
}

func TestJobEventsEmpty(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	c := NewForTest(nil, cs)

	got, err := c.JobEvents(context.Background(), "luncur-system", "build-absent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("JobEvents(no events) = %v, want empty", got)
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

func TestSetDeploymentImage(t *testing.T) {
	c, log := fakeClient(t)
	if err := c.SetDeploymentImage(context.Background(), "luncur-system", "luncur", "luncur", "x:y"); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(*log), *log)
	}
	rec := (*log)[0]
	if rec.verb != "patch" || rec.resource != "deployments" {
		t.Fatalf("want patch deployments, got %+v", rec)
	}
	if rec.namespace != "luncur-system" || rec.name != "luncur" {
		t.Fatalf("want luncur-system/luncur, got ns=%s name=%s", rec.namespace, rec.name)
	}
	if rec.patchType != "application/strategic-merge-patch+json" {
		t.Fatalf("want strategic-merge patch type, got %s", rec.patchType)
	}
	if !strings.Contains(string(rec.patch), `"image":"x:y"`) || !strings.Contains(string(rec.patch), `"name":"luncur"`) {
		t.Fatalf("patch missing expected fields: %s", rec.patch)
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

func TestStatefulSetReady(t *testing.T) {
	sts := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": "addon-db1", "namespace": "proj"},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	dyn := newFakeDyn(t, sts) // reuse the file's existing fake-dynamic constructor
	c := NewForTest(dyn, nil)
	ok, err := c.StatefulSetReady(context.Background(), "proj", "addon-db1")
	if err != nil || !ok {
		t.Fatalf("ready = %v err=%v", ok, err)
	}
	ok, err = c.StatefulSetReady(context.Background(), "proj", "absent")
	if err != nil || ok {
		t.Fatalf("absent: ready=%v err=%v, want false nil", ok, err)
	}
}

func TestClientImplementsPodExecer(t *testing.T) {
	var _ PodExecer = (*Client)(nil)
	c := NewForTest(nil, nil)
	err := c.ExecPod(context.Background(), "ns", "pod", "c", []string{"true"}, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exec unavailable") {
		t.Fatalf("cfg-less exec: %v", err)
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

func podMetricsObj(name, app, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata": map[string]any{
			"name": name, "namespace": "proj",
			"labels": map[string]any{"app.kubernetes.io/name": app},
		},
		"containers": []any{
			map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": mem}},
		},
	}}
}

// podMetricsGVR is gvrByKind["PodMetrics"], spelled out here since the fake
// dynamic client's tracker.Add path guesses a GVR by pluralizing the Kind
// ("PodMetrics" -> "podmetricses"), which doesn't match the real resource
// name ("pods"). Seeding must go through an explicit Create against this
// GVR instead of the constructor's varargs.
var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

func TestAppMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"},
	)
	ctx := context.Background()
	for _, obj := range []*unstructured.Unstructured{
		podMetricsObj("web-1", "web", "250m", "128Mi"),
		podMetricsObj("web-2", "web", "150m", "64Mi"),
		podMetricsObj("other-1", "other", "999m", "999Mi"),
	} {
		if _, err := dyn.Resource(podMetricsGVR).Namespace("proj").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	c := NewForTest(dyn, nil)
	m, ok := c.AppMetrics(ctx, "proj", "web")
	if !ok {
		t.Fatal("metrics unavailable")
	}
	if m.CPUMilli != 400 || m.MemoryMiB != 192 || m.Pods != 2 {
		t.Fatalf("metrics = %+v, want 400m/192MiB/2 pods", m)
	}
}

func readyCondNode(name string, ready bool) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: status},
		}},
	}
}

func TestNodesReadyAllReady(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(readyCondNode("n1", true))
	c := NewForTest(nil, cs)
	total, notReady, err := c.NodesReady(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(notReady) != 0 {
		t.Fatalf("NodesReady = (%d, %v), want (1, [])", total, notReady)
	}
}

func TestNodesReadyOneNotReady(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(readyCondNode("n1", true), readyCondNode("n2", false))
	c := NewForTest(nil, cs)
	total, notReady, err := c.NodesReady(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(notReady) != 1 || notReady[0] != "n2" {
		t.Fatalf("NodesReady = (%d, %v), want (2, [n2])", total, notReady)
	}
}

func TestListNodes(t *testing.T) {
	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp1",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "1.2.3.4"}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.32.5+k3s1"},
		},
	}
	agent := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "agent1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.32.5+k3s1"},
		},
	}
	cs := k8sfake.NewSimpleClientset(cp, agent)
	c := NewForTest(nil, cs)
	nodes, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("ListNodes = %+v, want 2 entries", nodes)
	}
	byName := map[string]NodeInfo{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	cpInfo, ok := byName["cp1"]
	if !ok || cpInfo.Role != "control-plane" || !cpInfo.Ready || cpInfo.IP != "1.2.3.4" || cpInfo.Version != "v1.32.5+k3s1" {
		t.Fatalf("cp1 = %+v", cpInfo)
	}
	agentInfo, ok := byName["agent1"]
	if !ok || agentInfo.Role != "agent" || agentInfo.Ready || agentInfo.IP != "10.0.0.5" {
		t.Fatalf("agent1 = %+v", agentInfo)
	}
}

func readyCondPod(namespace, name, label string, ready bool) *corev1.Pod {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{"app.kubernetes.io/name": label},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: status},
		}},
	}
}

func TestReadyPods(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		readyCondPod("kube-system", "traefik-1", "traefik", true),
		readyCondPod("kube-system", "traefik-2", "traefik", false),
		readyCondPod("kube-system", "other-1", "other", true),
	)
	c := NewForTest(nil, cs)
	ready, total, err := c.ReadyPods(context.Background(), "kube-system", "app.kubernetes.io/name=traefik")
	if err != nil {
		t.Fatal(err)
	}
	if ready != 1 || total != 2 {
		t.Fatalf("ReadyPods = (%d, %d), want (1, 2)", ready, total)
	}
}

func TestAppMetricsUnavailable(t *testing.T) {
	// The list kind must stay registered (an unregistered list kind panics
	// rather than erroring — see dynamicResourceClient.List), so force the
	// unavailable path with a reactor that fails the list call instead —
	// exactly the metrics-server-absent shape.
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"},
	)
	dyn.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the server could not find the requested resource")
	})
	c := NewForTest(dyn, nil)
	if _, ok := c.AppMetrics(context.Background(), "proj", "web"); ok {
		t.Fatal("want unavailable when metrics API missing")
	}
}

// gpuResourceList builds a corev1.ResourceList requesting n GPUs, for
// TestGPUBusyNodes' pod fixtures.
func gpuResourceList(n int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceName(render.GPUResource): *resource.NewQuantity(n, resource.DecimalSI),
	}
}

// TestGPUBusyNodes covers the three cases the per-instance idle loop cares
// about: a GPU pod running on a node marks that node busy; a non-GPU pod on
// another node is irrelevant; and a Pending GPU pod with no NodeName yet
// (unscheduled) is recorded under the "" key so callers can freeze destroys.
func TestGPUBusyNodes(t *testing.T) {
	gpuRunning := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-running", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Limits: gpuResourceList(1)},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	nonGPURunning := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   "node-b",
			Containers: []corev1.Container{{Name: "main"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	gpuPendingUnscheduled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-pending", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Requests: gpuResourceList(1)},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	cs := k8sfake.NewSimpleClientset(gpuRunning, nonGPURunning, gpuPendingUnscheduled)
	c := NewForTest(nil, cs)

	busy, err := c.GPUBusyNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"node-a": true, "": true}
	if len(busy) != len(want) {
		t.Fatalf("GPUBusyNodes = %+v, want %+v", busy, want)
	}
	for k, v := range want {
		if busy[k] != v {
			t.Fatalf("GPUBusyNodes = %+v, want %+v", busy, want)
		}
	}
}

// TestRunningJobPods covers the multi-node gang guard's core query: two of
// three pods labeled for the same Job are Running, one is still Pending.
func TestRunningJobPods(t *testing.T) {
	mkPod := func(name string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ns-train",
				Labels: map[string]string{"batch.kubernetes.io/job-name": "train-run-1"},
			},
			Status: corev1.PodStatus{Phase: phase},
		}
	}
	cs := k8sfake.NewSimpleClientset(
		mkPod("train-run-1-0", corev1.PodRunning),
		mkPod("train-run-1-1", corev1.PodRunning),
		mkPod("train-run-1-2", corev1.PodPending),
	)
	c := NewForTest(nil, cs)

	running, total, err := c.RunningJobPods(context.Background(), "ns-train", "train-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if running != 2 || total != 3 {
		t.Fatalf("RunningJobPods = (%d, %d), want (2, 3)", running, total)
	}
}

// TestHasWorkflowCRDAbsent/Present cover the argo engine's install
// preflight: false on an empty cluster, true once the Argo Workflows CRD
// object exists.
func TestHasWorkflowCRDAbsent(t *testing.T) {
	dyn := newFakeDyn(t)
	c := NewForTest(dyn, nil)
	ok, err := c.HasWorkflowCRD(context.Background())
	if err != nil || ok {
		t.Fatalf("HasWorkflowCRD(empty) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestHasWorkflowCRDPresent(t *testing.T) {
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "workflows.argoproj.io"},
	}}
	dyn := newFakeDyn(t, crd)
	c := NewForTest(dyn, nil)
	ok, err := c.HasWorkflowCRD(context.Background())
	if err != nil || !ok {
		t.Fatalf("HasWorkflowCRD(seeded) = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestApplyWorkflowPatchesTheWorkflowGVR mirrors
// TestApplyClusterRoleBindingSkipsNamespace: it asserts on the recorded SSA
// patch action rather than round-tripping through the fake dynamic
// client's real ObjectTracker. The tracker's apply-patch handling only
// computes a merge for types registered in its scheme (see dataStructFor
// for the typed kinds this package already round-trips); an arbitrary CRD
// kind like Workflow, decoded as unstructured.Unstructured, fails that
// merge with "unable to find api field in struct Unstructured" — a
// fake-client limitation, not a bug in Apply. Get/Delete (exercised below)
// don't go through that merge path and round-trip fine.
func TestApplyWorkflowPatchesTheWorkflowGVR(t *testing.T) {
	c, log := fakeClient(t)
	wf := []render.Object{{
		Kind: "Workflow",
		JSON: json.RawMessage(`{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","metadata":{"name":"pl-run1"},"spec":{"entrypoint":"main"}}`),
	}}
	if err := c.Apply(context.Background(), "luncur-ci-proj", wf); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(*log), *log)
	}
	rec := (*log)[0]
	if rec.verb != "patch" || rec.patchType != "application/apply-patch+yaml" {
		t.Fatalf("want SSA patch, got %+v", rec)
	}
	if rec.resource != "workflows" || rec.name != "pl-run1" || rec.namespace != "luncur-ci-proj" {
		t.Fatalf("bad action: %+v", rec)
	}
}

func workflowObj(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":      map[string]any{"entrypoint": "main"},
	}}
}

func TestGetWorkflowFound(t *testing.T) {
	dyn := newFakeDyn(t, workflowObj("pl-run1", "proj"))
	c := NewForTest(dyn, nil)
	got, found, err := c.GetWorkflow(context.Background(), "proj", "pl-run1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("want found=true")
	}
	if got["kind"] != "Workflow" {
		t.Fatalf("got = %+v, want kind=Workflow", got)
	}
}

func TestGetWorkflowAbsent(t *testing.T) {
	dyn := newFakeDyn(t)
	c := NewForTest(dyn, nil)
	got, found, err := c.GetWorkflow(context.Background(), "proj", "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if found || got != nil {
		t.Fatalf("GetWorkflow(absent) = (%+v, %v), want (nil, false)", got, found)
	}
}

func TestDeleteWorkflowIdempotent(t *testing.T) {
	dyn := newFakeDyn(t, workflowObj("pl-run1", "proj"))
	c := NewForTest(dyn, nil)
	if err := c.DeleteWorkflow(context.Background(), "proj", "pl-run1"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.GetWorkflow(context.Background(), "proj", "pl-run1"); err != nil || found {
		t.Fatalf("workflow still present after delete: found=%v err=%v", found, err)
	}
	// Idempotent: deleting an already-gone Workflow is not an error.
	if err := c.DeleteWorkflow(context.Background(), "proj", "pl-run1"); err != nil {
		t.Fatalf("delete missing workflow: %v", err)
	}
}

// TestDeleteJob covers both the happy path (Job is removed) and idempotency
// (deleting an already-gone Job is not an error).
func TestDeleteJob(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "train-run-1", Namespace: "ns-train"}}
	cs := k8sfake.NewSimpleClientset(job)
	c := NewForTest(nil, cs)

	if err := c.DeleteJob(context.Background(), "ns-train", "train-run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BatchV1().Jobs("ns-train").Get(context.Background(), "train-run-1", metav1.GetOptions{}); err == nil {
		t.Fatal("job still present after DeleteJob")
	}

	// Idempotent: deleting a missing job is success, not an error.
	if err := c.DeleteJob(context.Background(), "ns-train", "does-not-exist"); err != nil {
		t.Fatalf("delete missing job: %v", err)
	}
}

// TestTriggerCronJob covers the manual "run now" path: a Job is built from
// the live CronJob's JobTemplate, carries the app label and an owner
// reference back to the CronJob, and is named deterministically from the
// caller-supplied stamp.
func TestTriggerCronJob(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ns-cron", UID: "cj-uid-1"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "nightly"}},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers:    []corev1.Container{{Name: "app", Image: "nightly:1"}},
						},
					},
				},
			},
		},
	}
	cs := k8sfake.NewSimpleClientset(cj)
	c := NewForTest(nil, cs)

	name, err := c.TriggerCronJob(context.Background(), "ns-cron", "nightly", 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if name != "nightly-manual-1700000000" {
		t.Fatalf("job name: got %q", name)
	}

	job, err := cs.BatchV1().Jobs("ns-cron").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("created job not found: %v", err)
	}
	if job.Labels["app.kubernetes.io/name"] != "nightly" {
		t.Fatalf("job labels: %+v", job.Labels)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "CronJob" || job.OwnerReferences[0].Name != "nightly" {
		t.Fatalf("job owner references: %+v", job.OwnerReferences)
	}
	if job.Spec.Template.Spec.Containers[0].Image != "nightly:1" {
		t.Fatalf("job spec not copied from JobTemplate: %+v", job.Spec)
	}
}

// TestTriggerCronJobMissingCronJob covers the "not deployed yet" case: no
// live CronJob to build the manual run from.
func TestTriggerCronJobMissingCronJob(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	c := NewForTest(nil, cs)
	if _, err := c.TriggerCronJob(context.Background(), "ns-cron", "nightly", 1700000000); err == nil {
		t.Fatal("want error when the CronJob does not exist")
	}
}

// TestCronRuns covers the run-history listing: newest first, status derived
// from the Job's terminal counters, capped at 10.
func TestCronRuns(t *testing.T) {
	mk := func(name string, created time.Time, succeeded, failed int32, completed bool) *batchv1.Job {
		j := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ns-cron",
				Labels:            map[string]string{"app.kubernetes.io/name": "nightly"},
				CreationTimestamp: metav1.NewTime(created),
			},
			Status: batchv1.JobStatus{Succeeded: succeeded, Failed: failed},
		}
		start := metav1.NewTime(created)
		j.Status.StartTime = &start
		if completed {
			end := metav1.NewTime(created.Add(time.Minute))
			j.Status.CompletionTime = &end
		}
		return j
	}
	now := time.Now()
	older := mk("nightly-28392000", now.Add(-2*time.Hour), 1, 0, true)
	newer := mk("nightly-28392033", now.Add(-1*time.Hour), 0, 0, false)
	failed := mk("nightly-manual-1", now, 0, 1, false)
	other := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "other-run", Namespace: "ns-cron",
		Labels: map[string]string{"app.kubernetes.io/name": "other"},
	}}
	cs := k8sfake.NewSimpleClientset(older, newer, failed, other)
	c := NewForTest(nil, cs)

	runs, err := c.CronRuns(context.Background(), "ns-cron", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("want 3 runs (excluding other app's job), got %+v", runs)
	}
	// Newest first.
	if runs[0].Name != "nightly-manual-1" || runs[0].Status != "failed" {
		t.Fatalf("runs[0]: %+v", runs[0])
	}
	if runs[1].Name != "nightly-28392033" || runs[1].Status != "active" {
		t.Fatalf("runs[1]: %+v", runs[1])
	}
	if runs[2].Name != "nightly-28392000" || runs[2].Status != "succeeded" {
		t.Fatalf("runs[2]: %+v", runs[2])
	}
	if runs[2].StartTime == "" || runs[2].CompletionT == "" {
		t.Fatalf("runs[2] missing timestamps: %+v", runs[2])
	}
}

// clusterRoleRules reads back the live "luncur" ClusterRole's Rules from
// dyn, failing the test if it's missing or malformed.
func clusterRoleRules(t *testing.T, dyn *dynamicfake.FakeDynamicClient) []rbacv1.PolicyRule {
	t.Helper()
	u, err := dyn.Resource(gvrByKind["ClusterRole"]).Get(context.Background(), "luncur", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ClusterRole/luncur: %v", err)
	}
	b, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatal(err)
	}
	var cr rbacv1.ClusterRole
	if err := json.Unmarshal(b, &cr); err != nil {
		t.Fatal(err)
	}
	return cr.Rules
}

// TestEnsureClusterRoleCreatesWhenAbsent: EnsureClusterRole is the server's
// startup self-heal (internal/cli/serve.go) — on a fresh cluster (or one
// whose ClusterRole predates a feature) the "luncur" ClusterRole doesn't
// exist yet, and EnsureClusterRole must create it rather than error.
func TestEnsureClusterRoleCreatesWhenAbsent(t *testing.T) {
	dyn := newFakeDyn(t) // no seed objects: ClusterRole/luncur doesn't exist
	c := NewForTest(dyn, nil)
	want := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
		},
	}

	changed, err := c.EnsureClusterRole(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed=true creating an absent ClusterRole")
	}
	if got := clusterRoleRules(t, dyn); !reflect.DeepEqual(got, want.Rules) {
		t.Fatalf("rules after create = %+v, want %+v", got, want.Rules)
	}
}

// TestEnsureClusterRoleUpdatesWhenDrifted covers the field incident this
// function exists to fix: a live ClusterRole seeded with an older, narrower
// rule set (as if applied by a previous `luncur up`) must be brought up to
// date with the binary's current desired rules — e.g. adding the
// PodDisruptionBudget rule from #78 — without the operator re-running
// `luncur up`. A second call with matching rules is a no-op (changed=false).
func TestEnsureClusterRoleUpdatesWhenDrifted(t *testing.T) {
	old := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "luncur"},
		"rules": []any{
			map[string]any{
				"apiGroups": []any{""},
				"resources": []any{"pods"},
				"verbs":     []any{"get", "list", "watch"},
			},
		},
	}}
	dyn := newFakeDyn(t, old)
	c := NewForTest(dyn, nil)

	want := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		},
	}

	changed, err := c.EnsureClusterRole(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed=true updating a drifted ClusterRole")
	}
	if got := clusterRoleRules(t, dyn); !reflect.DeepEqual(got, want.Rules) {
		t.Fatalf("rules after update = %+v, want %+v", got, want.Rules)
	}

	// Second call against now-matching rules is a no-op.
	changed, err = c.EnsureClusterRole(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("want changed=false when rules already match")
	}
}
