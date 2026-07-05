package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	err := c.ExecPod(context.Background(), "ns", "pod", "c", []string{"true"}, io.Discard, io.Discard)
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
