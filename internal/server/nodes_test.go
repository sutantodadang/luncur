package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

type nodesResponse struct {
	Nodes []kube.NodeInfo `json:"nodes"`
}

// TestListNodesForbidsMember asserts a non-admin token gets 403.
func TestListNodesForbidsMember(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	member := seedUserToken(t, st, "pleb@b.co", "member")

	resp := doAuthed(t, "GET", srv.URL+"/v1/nodes", member, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member: want 403, got %d", resp.StatusCode)
	}
}

// TestListNodesNoKube asserts kube==nil returns 503 kubernetes_unavailable.
func TestListNodesNoKube(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/nodes", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

// TestListNodesHappyPath seeds one ready control-plane node and one not-ready
// agent node, and asserts both are reported with the right fields.
func TestListNodesHappyPath(t *testing.T) {
	st := newTestStore(t)
	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp1",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "1.2.3.4"}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.32.5+k3s1"},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
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
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, cs)})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/nodes", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out nodesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want 2 entries", out.Nodes)
	}
	byName := map[string]kube.NodeInfo{}
	for _, n := range out.Nodes {
		byName[n.Name] = n
	}
	cpInfo, ok := byName["cp1"]
	if !ok || cpInfo.Role != "control-plane" || !cpInfo.Ready || cpInfo.IP != "1.2.3.4" || cpInfo.Version != "v1.32.5+k3s1" {
		t.Fatalf("cp1 = %+v", cpInfo)
	}
	if cpInfo.CPUCapMilli != 4000 || cpInfo.MemCapMiB != 8192 || cpInfo.MetricsOK {
		t.Fatalf("cp1 capacity = %+v", cpInfo)
	}
	agentInfo, ok := byName["agent1"]
	if !ok || agentInfo.Role != "agent" || agentInfo.Ready || agentInfo.IP != "10.0.0.5" {
		t.Fatalf("agent1 = %+v", agentInfo)
	}
}

// TestListNodesWithMetrics asserts NodeMetrics usage merges into ListNodes
// output when the dynamic client is wired (nil dyn -> MetricsOK stays false,
// covered by TestListNodesHappyPath).
func TestListNodesWithMetrics(t *testing.T) {
	st := newTestStore(t)
	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	cs := k8sfake.NewSimpleClientset(cp)

	nodeMetricsGVR := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{nodeMetricsGVR: "NodeMetricsList"},
	)
	metrics := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata": map[string]any{"name": "cp1"},
		"usage":    map[string]any{"cpu": "250m", "memory": "512Mi"},
	}}
	if _, err := dyn.Resource(nodeMetricsGVR).Create(context.Background(), metrics, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(dyn, cs)})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/nodes", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out nodesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 1 {
		t.Fatalf("nodes = %+v, want 1 entry", out.Nodes)
	}
	n := out.Nodes[0]
	if !n.MetricsOK || n.CPUMilli != 250 || n.MemMiB != 512 {
		t.Fatalf("cp1 = %+v", n)
	}
}
