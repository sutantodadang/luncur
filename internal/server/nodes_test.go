package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

func TestNodeCordonAndDrain(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(readyNode("n1"), readyNode("n2"),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "ns"}, Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, cs), ExternalIP: "1.2.3.4"})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "m@b.co", "member")

	if resp := doAuthed(t, "POST", srv.URL+"/v1/nodes/n2/cordon", member, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member cordon = %d, want 403", resp.StatusCode)
	}
	resp := doAuthed(t, "POST", srv.URL+"/v1/nodes/n2/cordon", admin, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cordon = %d", resp.StatusCode)
	}
	// n2 cordoned: n1 is now the only schedulable node, so draining it
	// needs force.
	resp = doAuthed(t, "POST", srv.URL+"/v1/nodes/n1/drain", admin, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(b), "only schedulable node") {
		t.Fatalf("drain only node = %d %s", resp.StatusCode, b)
	}
	doAuthed(t, "POST", srv.URL+"/v1/nodes/n2/uncordon", admin, "").Body.Close()
	resp = doAuthed(t, "POST", srv.URL+"/v1/nodes/n1/drain", admin, `{"timeout":30}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := doAuthed(t, "GET", srv.URL+"/v1/nodes", admin, "")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), `"state":"done"`) && strings.Contains(string(b), `"evicted":1`) && strings.Contains(string(b), `"cordoned":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain never finished: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/nodes/nope/drain", admin, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown node drain = %d", resp.StatusCode)
	}
}
