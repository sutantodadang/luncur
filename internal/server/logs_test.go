package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// TestRuntimeLogs streams a single pod's logs as SSE. The fake clientset
// serves the canned string "fake logs" for any GetLogs request.
func TestRuntimeLogs(t *testing.T) {
	st := newTestStore(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "luncur-web",
			Labels:    map[string]string{"app.kubernetes.io/name": "api"},
		},
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, k8sfake.NewSimpleClientset(pod)), ExternalIP: "1.2.3.4"})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/logs", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "data: [web-1] fake logs") {
		t.Fatalf("missing pod log line:\n%s", body)
	}
	if !strings.Contains(body, "event: end") {
		t.Fatalf("missing end event:\n%s", body)
	}
}

// TestRuntimeLogsNoPods checks the app-has-no-pods case returns 404 no_pods.
func TestRuntimeLogsNoPods(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, k8sfake.NewSimpleClientset()), ExternalIP: "1.2.3.4"})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/logs", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Code != "no_pods" {
		t.Fatalf("want code no_pods, got %q", out.Error.Code)
	}
}
