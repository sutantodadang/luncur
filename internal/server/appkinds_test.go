package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

// mustReadBody reads and closes resp.Body, failing the test on error.
func mustReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// errCode decodes the error envelope's "code" field, failing the test if the
// body isn't a valid error envelope.
func errCode(t *testing.T, respBody []byte) string {
	t.Helper()
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode error envelope: %v (body: %s)", err, respBody)
	}
	return out.Error.Code
}

func TestCreateAppPerKind(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	// web (default kind, unchanged behavior).
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("web create: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// worker: no port.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"worker1","kind":"worker"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("worker create: want 201, got %d", resp.StatusCode)
	}
	var wa map[string]any
	json.NewDecoder(resp.Body).Decode(&wa)
	resp.Body.Close()
	if wa["kind"] != "worker" {
		t.Fatalf("worker kind: %v", wa["kind"])
	}

	// cron: requires schedule.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"bad-cron","kind":"cron"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("cron without schedule: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("cron create: want 201, got %d", resp.StatusCode)
	}
	var ca map[string]any
	json.NewDecoder(resp.Body).Decode(&ca)
	resp.Body.Close()
	if ca["kind"] != "cron" || ca["schedule"] != "0 3 * * *" {
		t.Fatalf("cron app: %+v", ca)
	}
}

func TestKindGateMatrix(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"worker1","kind":"worker"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()

	// domain add on worker -> 400 kind_mismatch.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/worker1/domains", admin, `{"hostname":"x.example.com"}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("domain on worker: want 400, got %d (%s)", resp.StatusCode, body)
	}
	if code := errCode(t, body); code != "kind_mismatch" {
		t.Fatalf("domain on worker: want kind_mismatch, got %q", code)
	}

	// scale replicas on cron -> 400 kind_mismatch.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/scale", admin, `{"replicas":3}`)
	body = mustReadBody(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("replicas on cron: want 400, got %d (%s)", resp.StatusCode, body)
	}
	if code := errCode(t, body); code != "kind_mismatch" {
		t.Fatalf("replicas on cron: want kind_mismatch, got %q", code)
	}

	// cpu-only scale on cron -> 200.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/scale", admin, `{"cpu":"250m"}`)
	body = mustReadBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("cpu-only scale on cron: want 200, got %d (%s)", resp.StatusCode, body)
	}

	// health set on cron -> 400 kind_mismatch.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/health", admin, `{"path":"/healthz"}`)
	body = mustReadBody(t, resp)
	if resp.StatusCode != 400 {
		t.Fatalf("health on cron: want 400, got %d (%s)", resp.StatusCode, body)
	}
	if code := errCode(t, body); code != "kind_mismatch" {
		t.Fatalf("health on cron: want kind_mismatch, got %q", code)
	}
}

func TestDeployWorkerAndCronHappyPath(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"worker1","kind":"worker"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()

	// Worker deploy: Deployment applied, no Service/Ingress.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/worker1/deploy", admin, `{"image":"registry/worker:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("worker deploy: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	joined := strings.Join(*actions, ",")
	if !strings.Contains(joined, "patch deployments") {
		t.Fatalf("worker deploy missing patch deployments: %s", joined)
	}
	if strings.Contains(joined, "patch services") || strings.Contains(joined, "patch ingresses") {
		t.Fatalf("worker deploy should not touch services/ingresses: %s", joined)
	}

	*actions = nil

	// Cron deploy: CronJob applied, no Service/Ingress/Deployment.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/nightly/deploy", admin, `{"image":"registry/nightly:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("cron deploy: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	joined = strings.Join(*actions, ",")
	if !strings.Contains(joined, "patch cronjobs") {
		t.Fatalf("cron deploy missing patch cronjobs: %s", joined)
	}
	if strings.Contains(joined, "patch services") || strings.Contains(joined, "patch ingresses") || strings.Contains(joined, "patch deployments") {
		t.Fatalf("cron deploy should only touch cronjobs: %s", joined)
	}
}

// TestRuntimeLogsForJobStylePodName checks pod log selection works
// regardless of app kind: a CronJob's pods get Job-generated names
// (app-<hash>-<hash>) but still carry the app.kubernetes.io/name label
// Render sets, so selection is unaffected by kind.
func TestRuntimeLogsForJobStylePodName(t *testing.T) {
	st := newTestStore(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-28392033-abcde",
			Namespace: "luncur-web",
			Labels:    map[string]string{"app.kubernetes.io/name": "nightly"},
		},
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, k8sfake.NewSimpleClientset(pod)), ExternalIP: "1.2.3.4"})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()

	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/nightly/logs", admin, "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body := mustReadBody(t, resp)
	if !strings.Contains(string(body), "nightly-28392033-abcde") {
		t.Fatalf("missing job-style pod name in stream:\n%s", body)
	}
}
