package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// webhookTestServer builds a kube+sealer+DataDir-backed test server whose
// fake Job Get reactor never reports a terminal status: WaitJob parks in its
// poll loop for the test's lifetime, so a build a webhook triggers
// deterministically stays "building" — no race against the async goroutine
// to observe the row right after the response.
func webhookTestServer(t *testing.T) (*httptestServer, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	dyn.PrependReactor("get", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": a.(ktesting.GetAction).GetName(), "namespace": "luncur-system"},
			"status":   map[string]any{},
		}}, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{
		Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer,
		ExternalIP: "1.2.3.4", DataDir: t.TempDir(),
	})
	return srv, st
}

func githubSig(secretHex string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secretHex))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, url string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeWebhookEnable(t *testing.T, resp *http.Response) (path, secretHex string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable webhook: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Enabled bool   `json:"enabled"`
		Path    string `json:"path"`
		Secret  string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || out.Path == "" || len(out.Secret) != 64 {
		t.Fatalf("bad enable response: %+v", out)
	}
	return out.Path, out.Secret
}

// TestWebhookEnableShowDisable exercises the authenticated manage endpoints:
// enable (secret returned once, 64 hex chars), rotate (new secret differs),
// show (never leaks the secret), and disable.
func TestWebhookEnableShowDisable(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()

	path1, secret1 := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, ""))
	if path1 != "/hooks/apps/proj/g" {
		t.Fatalf("path = %q", path1)
	}

	// Show never leaks the secret.
	showResp := doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, "")
	var shown map[string]any
	json.NewDecoder(showResp.Body).Decode(&shown)
	showResp.Body.Close()
	if shown["enabled"] != true || shown["path"] != path1 {
		t.Fatalf("show: %+v", shown)
	}
	if _, has := shown["secret"]; has {
		t.Fatalf("show leaked secret: %+v", shown)
	}

	// Rotate: a second enable regenerates the secret.
	_, secret2 := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, ""))
	if secret2 == secret1 {
		t.Fatal("rotate: want a new secret, got the same one")
	}

	// Disable.
	disResp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, "")
	var dis map[string]any
	json.NewDecoder(disResp.Body).Decode(&dis)
	disResp.Body.Close()
	if dis["enabled"] != false {
		t.Fatalf("disable: %+v", dis)
	}
	showResp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, "")
	json.NewDecoder(showResp.Body).Decode(&shown)
	showResp.Body.Close()
	if shown["enabled"] != false {
		t.Fatalf("show after disable: %+v", shown)
	}
}

// TestWebhookEnableGates checks the two enable-time rejections: a
// tarball-source app (400 bad_request) and an ejected app (409 app_ejected).
func TestWebhookEnableGates(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"tb","port":8080}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/tb/webhook", admin, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-git app: want 400, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env.Error.Code != "bad_request" {
		t.Fatalf("code = %q", env.Error.Code)
	}

	// Ejected git app.
	srv2, st2, _, _ := ejectTestServer(t)
	admin2 := seedUserToken(t, st2, "root@b.co", "admin")
	doAuthed(t, "POST", srv2.URL+"/v1/projects", admin2, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv2.URL+"/v1/projects/proj/apps", admin2,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()
	id := appID(t, st2, "proj", "g")
	if _, err := st2.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}
	doAuthed(t, "POST", srv2.URL+"/v1/projects/proj/apps/g/eject", admin2, "").Body.Close()
	assertAppEjected(t, doAuthed(t, "POST", srv2.URL+"/v1/projects/proj/apps/g/webhook", admin2, ""), "enable webhook on ejected app")
}

// TestWebhookTriggerAuth checks that every kind of auth failure — unknown
// app, disabled webhook, bad signature, missing headers, a rotated-out
// secret — answers with the byte-identical 401 body (no existence oracle).
func TestWebhookTriggerAuth(t *testing.T) {
	srv, st := webhookTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()
	path, secretHex := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, ""))

	body := []byte(`{"ref":"refs/heads/main"}`)
	var bodies [][]byte
	var statuses []int

	collect := func(resp *http.Response) {
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, resp.StatusCode)
		bodies = append(bodies, b)
	}

	// Unknown app.
	collect(postWebhook(t, srv.URL+"/hooks/apps/proj/nosuchapp",
		map[string]string{"X-Hub-Signature-256": githubSig(secretHex, body)}, body))
	// Bad signature.
	collect(postWebhook(t, srv.URL+path,
		map[string]string{"X-Hub-Signature-256": githubSig("wrong-secret", body)}, body))
	// Missing headers entirely.
	collect(postWebhook(t, srv.URL+path, nil, body))
	// Rotate the secret, then use the OLD one — must fail identically.
	oldSig := githubSig(secretHex, body)
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, "").Body.Close()
	collect(postWebhook(t, srv.URL+path, map[string]string{"X-Hub-Signature-256": oldSig}, body))
	// Disabled webhook.
	doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, "").Body.Close()
	collect(postWebhook(t, srv.URL+path, map[string]string{"X-Hub-Signature-256": oldSig}, body))

	for i, code := range statuses {
		if code != http.StatusUnauthorized {
			t.Fatalf("case %d: status = %d, want 401", i, code)
		}
	}
	for i := 1; i < len(bodies); i++ {
		if string(bodies[i]) != string(bodies[0]) {
			t.Fatalf("case %d body %q != case 0 body %q — auth failures must be identical", i, bodies[i], bodies[0])
		}
	}

	if n, err := st.CountDeployments(appID(t, st, "proj", "g")); err != nil || n != 0 {
		t.Fatalf("no deploy should have happened: n=%d err=%v", n, err)
	}
}

// TestWebhookTriggerEvents drives the post-auth event/branch/dedupe logic:
// ping, wrong branch, a valid push (building deployment created), the same
// push again (deduped against the in-progress build), and a GitLab non-push
// event skipped.
func TestWebhookTriggerEvents(t *testing.T) {
	srv, st := webhookTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()
	path, secretHex := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, ""))
	appDBID := appID(t, st, "proj", "g")

	sign := func(body []byte) map[string]string {
		return map[string]string{"X-Hub-Signature-256": githubSig(secretHex, body)}
	}

	// ping: 200, no deploy.
	pingBody := []byte(`{"zen":"hi"}`)
	resp := postWebhook(t, srv.URL+path, mergeHeaders(sign(pingBody), map[string]string{"X-GitHub-Event": "ping"}), pingBody)
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out["pong"] != true {
		t.Fatalf("ping: status=%d body=%v", resp.StatusCode, out)
	}
	if n, _ := st.CountDeployments(appDBID); n != 0 {
		t.Fatalf("ping should not deploy, count=%d", n)
	}

	// Wrong branch: 200 skipped, no deploy.
	wrongBranch := []byte(`{"ref":"refs/heads/other"}`)
	resp = postWebhook(t, srv.URL+path, sign(wrongBranch), wrongBranch)
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out["skipped"] != "branch" {
		t.Fatalf("wrong branch: status=%d body=%v", resp.StatusCode, out)
	}
	if n, _ := st.CountDeployments(appDBID); n != 0 {
		t.Fatalf("wrong branch should not deploy, count=%d", n)
	}

	// Correct branch push: 202, building deployment created.
	push := []byte(`{"ref":"refs/heads/main"}`)
	resp = postWebhook(t, srv.URL+path, sign(push), push)
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push: status=%d body=%v", resp.StatusCode, out)
	}
	depID := out["deployment_id"].(string)
	d, err := st.GetDeployment(depID)
	if err != nil || d.Status != "building" {
		t.Fatalf("deployment: %+v %v", d, err)
	}

	// Same push again while the first build is still in progress: deduped.
	resp = postWebhook(t, srv.URL+path, sign(push), push)
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || out["skipped"] != "in_progress" {
		t.Fatalf("dedupe: status=%d body=%v", resp.StatusCode, out)
	}
	if out["deployment_id"].(string) != depID {
		t.Fatalf("dedupe: deployment_id = %v, want %s", out["deployment_id"], depID)
	}
	if n, _ := st.CountDeployments(appDBID); n != 1 {
		t.Fatalf("dedupe should not create a new deployment, count=%d", n)
	}

	// GitLab non-push event: 200 skipped, no new deploy.
	glBody := []byte(`{"object_kind":"tag_push","ref":"refs/heads/main"}`)
	resp = postWebhook(t, srv.URL+path, map[string]string{"X-Gitlab-Token": secretHex}, glBody)
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out["skipped"] != "event" {
		t.Fatalf("gitlab non-push: status=%d body=%v", resp.StatusCode, out)
	}
	if n, _ := st.CountDeployments(appDBID); n != 1 {
		t.Fatalf("gitlab non-push should not deploy, count=%d", n)
	}
}

// TestWebhookTriggerGitLabPushAndEjected checks a fresh GitLab-token push
// (on a second app, so it doesn't race the first app's in-progress build)
// and the ejected-app 409.
func TestWebhookTriggerGitLabPushAndEjected(t *testing.T) {
	srv, st := webhookTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g2","port":8080,"git_url":"https://x/y.git"}`).Body.Close()
	path, secretHex := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g2/webhook", admin, ""))

	push := []byte(`{"object_kind":"push","ref":"refs/heads/main"}`)
	resp := postWebhook(t, srv.URL+path, map[string]string{"X-Gitlab-Token": secretHex}, push)
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || out["deployment_id"] == nil {
		t.Fatalf("gitlab push: status=%d body=%v", resp.StatusCode, out)
	}

	// Eject then retry: 409.
	id := appID(t, st, "proj", "g2")
	if err := st.SetAppEjected(id); err != nil {
		t.Fatal(err)
	}
	resp = postWebhook(t, srv.URL+path, map[string]string{"X-Gitlab-Token": secretHex}, push)
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("ejected: status=%d body=%v", resp.StatusCode, out)
	}
}

// TestWebhookTriggerOversizedBody checks the 1 MiB cap rejects a too-large
// body without ever reaching the deploy logic.
func TestWebhookTriggerOversizedBody(t *testing.T) {
	srv, st := webhookTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()
	path, secretHex := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, ""))

	huge := make([]byte, (1<<20)+1)
	resp := postWebhook(t, srv.URL+path, map[string]string{"X-Hub-Signature-256": githubSig(secretHex, huge)}, huge)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("oversized body: status=%d, want 401", resp.StatusCode)
	}
	if n, err := st.CountDeployments(appID(t, st, "proj", "g")); err != nil || n != 0 {
		t.Fatalf("oversized body must not deploy: n=%d err=%v", n, err)
	}
}

func mergeHeaders(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
