package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// TestLogBounds checks the ?tail= and ?since= query param parsing: absent
// params mean unbounded (zero values), valid params convert to line count /
// seconds, and invalid params return an error.
func TestLogBounds(t *testing.T) {
	cases := []struct {
		name        string
		query       string
		wantTail    int64
		wantSince   int64
		wantErr     bool
	}{
		{"no params", "/x", 0, 0, false},
		{"tail only", "/x?tail=500", 500, 0, false},
		{"since minutes", "/x?since=15m", 0, 900, false},
		{"since seconds", "/x?since=90s", 0, 90, false},
		{"bad tail", "/x?tail=abc", 0, 0, true},
		{"negative tail", "/x?tail=-5", 0, 0, true},
		{"zero tail", "/x?tail=0", 0, 0, true},
		{"bad since", "/x?since=xyz", 0, 0, true},
		{"negative since", "/x?since=-5m", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.query, nil)
			tail, since, err := logBounds(r)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tail != c.wantTail || since != c.wantSince {
				t.Fatalf("got tail=%d since=%d, want tail=%d since=%d", tail, since, c.wantTail, c.wantSince)
			}
		})
	}
}

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

// TestRuntimeLogsWithBounds checks that valid ?tail=&since= params don't
// break the stream path. The fake clientset ignores PodLogOptions, so this
// only proves the params are accepted and plumbed through without error.
func TestRuntimeLogsWithBounds(t *testing.T) {
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

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/logs?tail=100&since=5m", admin, "")
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
}

// TestRuntimeLogsBadTail checks an invalid ?tail= returns 400 bad_request.
func TestRuntimeLogsBadTail(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, k8sfake.NewSimpleClientset()), ExternalIP: "1.2.3.4"})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/logs?tail=abc", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Code != "bad_request" {
		t.Fatalf("want code bad_request, got %q", out.Error.Code)
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
