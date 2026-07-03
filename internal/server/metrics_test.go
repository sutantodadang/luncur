package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// podMetricsGVR is gvrByKind["PodMetrics"] in internal/kube — spelled out
// here since the fake dynamic client's constructor guesses a GVR by
// pluralizing the Kind ("PodMetrics" -> "podmetricses"), which doesn't match
// the real resource name ("pods"); seed objects must be Created against this
// GVR explicitly instead of passed to the constructor's varargs.
var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

func podMetricsObj(name, namespace, app, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata": map[string]any{
			"name": name, "namespace": namespace,
			"labels": map[string]any{"app.kubernetes.io/name": app},
		},
		"containers": []any{
			map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": mem}},
		},
	}}
}

// metricsTestServer builds a server whose kube layer carries two PodMetrics
// objects for app "web" (250m/128Mi + 150m/64Mi -> 400m/192Mi/2 pods) plus a
// Deployment reporting 1/1 ready replicas — mirrors how apps_test.go's
// kubeServer and addons_test.go's addonTestServer build their fakes, adding
// the PodMetrics list-kind map the metrics endpoint needs.
func metricsTestServer(t *testing.T) (*httptestServer, *store.Store, store.Project, store.App) {
	t.Helper()
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"},
	)
	ctx := context.Background()
	for _, obj := range []*unstructured.Unstructured{
		podMetricsObj("web-1", p.Namespace, a.Name, "250m", "128Mi"),
		podMetricsObj("web-2", p.Namespace, a.Name, "150m", "64Mi"),
	} {
		if _, err := dyn.Resource(podMetricsGVR).Namespace(p.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": a.Name, "namespace": p.Namespace},
		"spec":     map[string]any{"replicas": int64(1)},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	deploymentGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	if _, err := dyn.Resource(deploymentGVR).Namespace(p.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer, ExternalIP: "1.2.3.4"})
	return srv, st, p, a
}

func TestAppMetricsEndpoint(t *testing.T) {
	srv, st, _, a := metricsTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	for i := 0; i < 3; i++ {
		if _, err := st.CreateDeployment(a.ID, "live", "img", 0); err != nil {
			t.Fatal(err)
		}
	}

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/metrics", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"available": true, "cpu_millicores": float64(400), "memory_mib": float64(192),
		"pods": float64(2), "ready_replicas": float64(1), "desired_replicas": float64(1),
		"deploy_count": float64(3),
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %s = %v, want %v (full: %+v)", k, got[k], v, got)
		}
	}
}

func TestAppMetricsWithoutKube(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	id := appID(t, st, "proj", "web")
	if _, err := st.CreateDeployment(id, "live", "img", 0); err != nil {
		t.Fatal(err)
	}

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/metrics", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"available": false, "cpu_millicores": float64(0), "memory_mib": float64(0),
		"pods": float64(0), "ready_replicas": float64(0), "desired_replicas": float64(0),
		"deploy_count": float64(1),
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %s = %v, want %v (full: %+v)", k, got[k], v, got)
		}
	}
}
