package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// podsResponse mirrors the /pods endpoint's JSON envelope.
type podsResponse struct {
	Pods []kube.PodInfo `json:"pods"`
}

func TestAppPodsEndpoint(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	startedAt := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: p.Namespace,
			Labels: map[string]string{"app.kubernetes.io/name": a.Name},
		},
		Spec: corev1.PodSpec{NodeName: "cp1"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			StartTime:  &startedAt,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 2, Ready: true},
			},
		},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-2", Namespace: p.Namespace,
			Labels: map[string]string{"app.kubernetes.io/name": a.Name},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}},
		},
	}
	cs := k8sfake.NewSimpleClientset(running, pending)

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"},
	)
	ctx := context.Background()
	if _, err := dyn.Resource(podMetricsGVR).Namespace(p.Namespace).Create(
		ctx, podMetricsObj("web-1", p.Namespace, a.Name, "250m", "128Mi"), metav1.CreateOptions{},
	); err != nil {
		t.Fatal(err)
	}

	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(dyn, cs)})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/pods", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out podsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pods) != 2 {
		t.Fatalf("pods = %+v, want 2 entries", out.Pods)
	}
	byName := map[string]kube.PodInfo{}
	for _, pod := range out.Pods {
		byName[pod.Name] = pod
	}

	web1, ok := byName["web-1"]
	if !ok {
		t.Fatalf("missing web-1: %+v", out.Pods)
	}
	if web1.Phase != "Running" || !web1.Ready || web1.Restarts != 2 || web1.Node != "cp1" {
		t.Fatalf("web-1 = %+v", web1)
	}
	if !web1.MetricsOK || web1.CPUMilli != 250 || web1.MemoryMiB != 128 {
		t.Fatalf("web-1 metrics = %+v", web1)
	}

	web2, ok := byName["web-2"]
	if !ok {
		t.Fatalf("missing web-2: %+v", out.Pods)
	}
	if web2.Phase != "Pending" || web2.Reason != "CrashLoopBackOff" || web2.MetricsOK {
		t.Fatalf("web-2 = %+v", web2)
	}
}

// TestUIAppPodsCard asserts the app page's Pods card renders a running
// pod's name.
func TestUIAppPodsCard(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: p.Namespace,
			Labels: map[string]string{"app.kubernetes.io/name": a.Name},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := k8sfake.NewSimpleClientset(running)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"},
	)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(dyn, cs)})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if !strings.Contains(body, "Pods") {
		t.Fatalf("app page missing Pods heading, got: %s", body)
	}
	if !strings.Contains(body, "web-1") {
		t.Fatalf("app page missing pod name, got: %s", body)
	}
}

func TestAppPodsNoKube(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/pods", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}
