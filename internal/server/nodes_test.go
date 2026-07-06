package server

import (
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	agentInfo, ok := byName["agent1"]
	if !ok || agentInfo.Role != "agent" || agentInfo.Ready || agentInfo.IP != "10.0.0.5" {
		t.Fatalf("agent1 = %+v", agentInfo)
	}
}
