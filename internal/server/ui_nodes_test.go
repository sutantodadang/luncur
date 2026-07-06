package server

import (
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// TestUINodesPage checks the admin sees the seeded node's name/role/status,
// a member gets 404, and a nil kube client renders the "not configured"
// message instead of erroring.
func TestUINodesPage(t *testing.T) {
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
	cs := k8sfake.NewSimpleClientset(cp)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, cs)})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/nodes", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/nodes: want 200, got %d", status)
	}
	if !strings.Contains(body, "cp1") {
		t.Fatalf("nodes page missing node name, got: %s", body)
	}
	if !strings.Contains(body, "control-plane") {
		t.Fatalf("nodes page missing role, got: %s", body)
	}
	if !strings.Contains(body, "ready") {
		t.Fatalf("nodes page missing status, got: %s", body)
	}

	status, _ = getUIPage(t, client, srv.URL, "/ui/nodes", uiSessionCookie(t, st, member.ID))
	if status != http.StatusNotFound {
		t.Fatalf("member GET /ui/nodes: want 404, got %d", status)
	}
}

// TestUINodesPageNoKube asserts a nil kube client renders the "not
// configured" message instead of erroring.
func TestUINodesPageNoKube(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/nodes", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/nodes: want 200, got %d", status)
	}
	if !strings.Contains(body, "kubernetes is not configured") {
		t.Fatalf("nodes page missing not-configured message, got: %s", body)
	}
}
