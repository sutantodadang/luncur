package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
)

// TestMetricRingWrap asserts the ring keeps only the newest monitorWindow
// samples, oldest-first, once it has wrapped.
func TestMetricRingWrap(t *testing.T) {
	var r metricRing
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	total := monitorWindow + 5
	for i := 0; i < total; i++ {
		r.add(metricSample{At: base.Add(time.Duration(i) * time.Second), CPUMilli: int64(i)})
	}
	snap := r.snapshot()
	if len(snap) != monitorWindow {
		t.Fatalf("snapshot len = %d, want %d", len(snap), monitorWindow)
	}
	if snap[0].CPUMilli != 5 {
		t.Fatalf("first sample = %d, want 5 (sample #6)", snap[0].CPUMilli)
	}
	if snap[len(snap)-1].CPUMilli != int64(total-1) {
		t.Fatalf("last sample = %d, want %d", snap[len(snap)-1].CPUMilli, total-1)
	}
}

// TestSparkPoints asserts the exact points string sparkPoints emits for a
// known 3-sample series, and that <2 samples renders "".
func TestSparkPoints(t *testing.T) {
	samples := []metricSample{{CPUMilli: 0}, {CPUMilli: 50}, {CPUMilli: 100}}
	pick := func(s metricSample) int64 { return s.CPUMilli }
	got := sparkPoints(samples, pick, 600, 48)
	want := "0,47 299,24 599,1"
	if got != want {
		t.Fatalf("sparkPoints = %q, want %q", got, want)
	}
	if got := sparkPoints(samples[:1], pick, 600, 48); got != "" {
		t.Fatalf("single sample: sparkPoints = %q, want \"\"", got)
	}
	if got := sparkPoints(nil, pick, 600, 48); got != "" {
		t.Fatalf("no samples: sparkPoints = %q, want \"\"", got)
	}
}

// TestSampleMetricsRecords drives sampleMetrics directly against a fake
// dynamic/clientset pair: one PodMetrics object for app "web" is sampled
// twice, then removed, and a third sample must zero-fill instead of
// repeating the last real value.
func TestSampleMetricsRecords(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(p.ID, "web", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}

	nodeMetricsGVR := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		podMetricsGVR: "PodMetricsList", nodeMetricsGVR: "NodeMetricsList",
	})
	ctx := context.Background()
	obj := podMetricsObj("web-1", p.Namespace, "web", "250m", "128Mi")
	if _, err := dyn.Resource(podMetricsGVR).Namespace(p.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := k8sfake.NewSimpleClientset()

	s := newServer(Deps{Store: st, Kube: kube.NewForTest(dyn, cs)})
	key := p.Namespace + "/web"

	s.sampleMetrics(ctx)
	s.sampleMetrics(ctx)
	samples := s.mon.appSamples(key)
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want 2", samples)
	}
	for _, sm := range samples {
		if sm.CPUMilli != 250 {
			t.Fatalf("sample CPUMilli = %d, want 250", sm.CPUMilli)
		}
	}

	if err := dyn.Resource(podMetricsGVR).Namespace(p.Namespace).Delete(ctx, "web-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.sampleMetrics(ctx)
	samples = s.mon.appSamples(key)
	if len(samples) != 3 {
		t.Fatalf("samples = %+v, want 3", samples)
	}
	last := samples[len(samples)-1]
	if last.CPUMilli != 0 || last.MemoryMiB != 0 {
		t.Fatalf("zero-fill sample = %+v, want zero", last)
	}
}

// TestAppMetricsHistoryEndpoint seeds the monitor directly (bypassing the
// sampler) and asserts the JSON history endpoint reports it, with an empty
// ring rendering "samples":[] rather than null.
func TestAppMetricsHistoryEndpoint(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(p.ID, "web", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	admin := seedUserToken(t, st, "root@b.co", "admin")

	// Empty ring: samples must serialize as [] not null.
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/metrics/history", admin, "")
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(rawBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, `"samples":[]`) {
		t.Fatalf("empty history body = %s, want \"samples\":[]", body)
	}

	now := time.Now()
	key := p.Namespace + "/web"
	s.mon.record(now, map[string]kube.AppMetrics{key: {CPUMilli: 111, MemoryMiB: 222}}, nil)
	s.mon.record(now.Add(15*time.Second), map[string]kube.AppMetrics{key: {CPUMilli: 111, MemoryMiB: 222}}, nil)

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/web/metrics/history", admin, "")
	defer resp.Body.Close()
	var out struct {
		Samples []struct {
			CPU int64 `json:"cpu_millicores"`
			Mem int64 `json:"memory_mib"`
		} `json:"samples"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Samples) != 2 || out.Samples[0].CPU != 111 || out.Samples[0].Mem != 222 {
		t.Fatalf("samples = %+v", out.Samples)
	}
}

// TestUIAppChartFragment asserts the chart fragment reports "collecting"
// with <2 samples and renders an svg polyline once seeded with >=2.
func TestUIAppChartFragment(t *testing.T) {
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(p.ID, "web", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web/chart", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if !strings.Contains(body, "collecting") {
		t.Fatalf("chart fragment with <2 samples = %s, want \"collecting\"", body)
	}

	now := time.Now()
	key := p.Namespace + "/web"
	s.mon.record(now, map[string]kube.AppMetrics{key: {CPUMilli: 50, MemoryMiB: 64}}, nil)
	s.mon.record(now.Add(15*time.Second), map[string]kube.AppMetrics{key: {CPUMilli: 80, MemoryMiB: 96}}, nil)

	status, body = getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web/chart", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "polyline") {
		t.Fatalf("chart fragment with samples = %s, want svg+polyline", body)
	}
}

// TestUINodeChartsFragment asserts the nodes charts fragment includes the
// seeded node's name and an svg once the monitor has samples for it.
func TestUINodeChartsFragment(t *testing.T) {
	st := newTestStore(t)
	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	cs := k8sfake.NewSimpleClientset(cp)
	s := newServer(Deps{Store: st, Kube: kube.NewForTest(nil, cs)})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	now := time.Now()
	s.mon.record(now, nil, []kube.NodeInfo{{Name: "cp1", MetricsOK: true, CPUMilli: 100, MemMiB: 200}})
	s.mon.record(now.Add(15*time.Second), nil, []kube.NodeInfo{{Name: "cp1", MetricsOK: true, CPUMilli: 150, MemMiB: 250}})

	status, body := getUIPage(t, client, srv.URL, "/ui/nodes/charts", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if !strings.Contains(body, "cp1") || !strings.Contains(body, "<svg") {
		t.Fatalf("node charts fragment = %s, want cp1 + svg", body)
	}
}

// crashPod builds a fake pod labeled for app under namespace ns, with one
// container status reporting restarts restarts.
func crashPod(ns, app string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app + "-0",
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/name": app},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: restarts},
			},
		},
	}
}

// TestMonitorNotifiesCrashLoop drives sampleMetrics directly: a fake pod for
// app "api" starts at 0 restarts (baseline, no alert), jumps to 3 restarts
// (delta >= 3 -> exactly one app_unhealthy delivery), then holds at 3 (no
// further delta -> nothing sent, exercising the same suppression path the
// cooldown uses).
func TestMonitorNotifiesCrashLoop(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(p.ID, "api", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}

	pod := crashPod(p.Namespace, "api", 0)
	cs := k8sfake.NewSimpleClientset(pod)

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, Kube: kube.NewForTest(nil, cs)})
	setSealedNotifyURL(t, s, ts.URL)

	ctx := context.Background()

	// Tick 1: baseline (0 restarts) — nothing to compare against yet.
	s.sampleMetrics(ctx)
	select {
	case b := <-ch:
		t.Fatalf("unexpected notification on baseline tick: %s", b)
	case <-time.After(200 * time.Millisecond):
	}

	// Bump restarts to 3 (delta >= 3) and tick again — one delivery.
	pod.Status.ContainerStatuses[0].RestartCount = 3
	if _, err := cs.CoreV1().Pods(p.Namespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s.sampleMetrics(ctx)
	b := recvNotify(t, ch, 2*time.Second)
	if !strings.Contains(string(b), `"event":"app_unhealthy"`) {
		t.Fatalf("body = %s, want app_unhealthy", b)
	}

	// Tick again with restarts unchanged — delta 0, suppressed.
	s.sampleMetrics(ctx)
	select {
	case b := <-ch:
		t.Fatalf("unexpected second notification: %s", b)
	case <-time.After(200 * time.Millisecond):
	}
}
