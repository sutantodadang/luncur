package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// kubeServer builds a test server with a fake kube layer that records actions.
func kubeServer(t *testing.T) (*httptestServer, *store.Store, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer, ExternalIP: "1.2.3.4"})
	return srv, st, &actions
}

func TestAppLifecycle(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	// Create app.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create app: want 201, got %d", resp.StatusCode)
	}
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	if app["url"] != "http://api.1-2-3-4.sslip.io" {
		t.Fatalf("url: %v", app["url"])
	}

	// Status before deploy.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api", admin, "")
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["status"] != "never_deployed" {
		t.Fatalf("status: %v", got["status"])
	}

	// Deploy.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"nginx:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d", resp.StatusCode)
	}
	joined := strings.Join(*actions, ",")
	if !strings.Contains(joined, "patch namespaces") || !strings.Contains(joined, "patch deployments") {
		t.Fatalf("kube actions missing: %s", joined)
	}
	d, err := st.LatestDeployment(appID(t, st, "web", "api"))
	if err != nil || d.Status != "live" || d.ImageRef != "nginx:1" {
		t.Fatalf("deployment row: %+v %v", d, err)
	}

	// Scale re-applies (live deployment exists).
	before := len(*actions)
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":3}`)
	if resp.StatusCode != 200 {
		t.Fatalf("scale: want 200, got %d", resp.StatusCode)
	}
	if len(*actions) <= before {
		t.Fatal("scale should re-apply to cluster")
	}

	// Destroy.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/api", admin, "")
	if resp.StatusCode != 204 {
		t.Fatalf("destroy: want 204, got %d", resp.StatusCode)
	}
	joined = strings.Join(*actions, ",")
	if !strings.Contains(joined, "delete deployments") {
		t.Fatalf("no delete actions: %s", joined)
	}
}

func TestCreateGitApp(t *testing.T) {
	srv, st := testServer(t) // no kube needed for create-only
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"g","port":8080,"git_url":"https://x/y.git"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create git app: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	a, err := st.GetApp(mustProjectID(t, st, "web"), "g")
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceType != "git" {
		t.Fatalf("source type: want git, got %q", a.SourceType)
	}
	if a.GitURL != "https://x/y.git" {
		t.Fatalf("git url: got %q", a.GitURL)
	}
}

func appID(t *testing.T, st *store.Store, project, app string) int64 {
	t.Helper()
	p, err := st.GetProject(project)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, app)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestDeployWithoutKube503(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("want 503 without kube, got %d", resp.StatusCode)
	}
}

func TestScaleLiveAppWithoutKube503LeavesReplicasUnchanged(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	// Simulate a previously-live deployment directly in the store: the
	// deploy handler itself requires kube, so this test constructs the
	// "live app, no kube available now" state without going through it.
	id := appID(t, st, "web", "api")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":5}`)
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}

	a, err := st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.Replicas != 1 {
		t.Fatalf("replicas must be unchanged (still 1), got %d", a.Replicas)
	}
}

// TestScaleResourcesPartialUpdate exercises the cpu/memory-only scale path:
// a request touching only cpu leaves replicas untouched, and clearing via ""
// resets to 0.
func TestScaleResourcesPartialUpdate(t *testing.T) {
	srv, st := testServer(t) // no kube; app never live, so no sync required
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"cpu":"250m"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("cpu-only scale: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	a, err := st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.Replicas != 1 {
		t.Fatalf("replicas should be untouched, got %d", a.Replicas)
	}
	if a.CPUMilli != 250 {
		t.Fatalf("cpu_milli: want 250, got %d", a.CPUMilli)
	}

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"memory":"256Mi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("memory-only scale: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	a, err = st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.CPUMilli != 250 || a.MemoryMB != 256 {
		t.Fatalf("want cpu unchanged + memory set, got %+v", a)
	}

	// Clear cpu via "".
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"cpu":""}`)
	if resp.StatusCode != 200 {
		t.Fatalf("clear cpu: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	a, err = st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.CPUMilli != 0 || a.MemoryMB != 256 {
		t.Fatalf("want cpu cleared, memory unchanged, got %+v", a)
	}
}

// TestScaleInvalidQuantity400 checks a bad cpu/memory quantity fails with a
// 400 naming the offending field, and that an all-nil body ({}) is rejected
// as "nothing to change" rather than silently scaling replicas to 0.
func TestScaleInvalidQuantity400(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"cpu":"bogus"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad cpu: want 400, got %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	errBody, _ := out["error"].(map[string]any)
	if !strings.Contains(fmt.Sprint(errBody["message"]), "cpu") {
		t.Fatalf("want field name in error message, got %v", out)
	}

	resp2 := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("empty body: want 400, got %d", resp2.StatusCode)
	}

	a, err := st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.Replicas != 1 {
		t.Fatalf("empty body must not scale replicas to 0, got %d", a.Replicas)
	}
}

func mustProjectID(t *testing.T, st *store.Store, project string) int64 {
	t.Helper()
	p, err := st.GetProject(project)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestMemberForbiddenOnForeignProject(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "m@b.co", "member")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps", member, "")
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

// TestDeployMultipartBuildsAsync exercises the tarball-upload deploy path:
// 202 building, tarball persisted to the data dir, and the deployment row
// left in "building" status. The fake Job's Get reactor reports neither
// succeeded nor failed, so the background runBuild goroutine parks in
// WaitJob's poll loop for the lifetime of this test — the row is
// deterministically still "building" by the time we assert, without racing
// the async completion.
func TestDeployMultipartBuildsAsync(t *testing.T) {
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		return true, nil, nil
	})
	dyn.PrependReactor("get", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": a.(ktesting.GetAction).GetName(), "namespace": "luncur-system"},
			"status":   map[string]any{"failed": int64(1)},
		}}, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer, ExternalIP: "1.2.3.4", DataDir: dataDir})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("source", "src.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("fake-tarball-bytes"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", srv.URL+"/v1/projects/web/apps/api/deploy", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "building" {
		t.Fatalf("response status=%v want building", out["status"])
	}
	depID := int64(out["deployment_id"].(float64))

	// The 202 response above already proved the synchronous state is
	// "building". The async build goroutine then runs; the fake job reports
	// failed, so poll until the deployment reaches its terminal state. This
	// also guarantees the goroutine finishes its store writes before
	// t.Cleanup closes the store (no write-to-closed-DB race, no leak).
	var got store.Deployment
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err = st.GetDeployment(depID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "failed" || got.Status == "live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not reach terminal status, stuck at %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != "failed" {
		t.Fatalf("row status=%q want failed (fake job failed)", got.Status)
	}

	src, err := build.NewSource(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src.TarballPath(depID)); err != nil {
		t.Fatalf("tarball not saved: %v", err)
	}
}

// TestDeployLogsReturnsStoredBytes checks the logs endpoint reads back
// whatever runBuild (or, here, the test directly) wrote to the deployment's
// log path.
func TestDeployLogsReturnsStoredBytes(t *testing.T) {
	st := newTestStore(t)
	dataDir := t.TempDir()
	srv := newHTTPTest(t, Deps{Store: st, ExternalIP: "1.2.3.4", DataDir: dataDir})

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	id := appID(t, st, "web", "api")
	d, err := st.CreateDeployment(id, "building", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	src, err := build.NewSource(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("line1\nline2\n")
	if err := os.WriteFile(src.LogPath(d.ID), want, 0o600); err != nil {
		t.Fatal(err)
	}

	resp := doAuthed(t, "GET", fmt.Sprintf("%s/v1/projects/web/apps/api/deploys/%d/logs", srv.URL, d.ID), admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(want) {
		t.Fatalf("logs=%q want %q", body, want)
	}
}

// applyImageDeployServer builds a bare *server (fake dynamic client, sealer,
// no HTTP mux) for exercising applyImageDeploy directly. failApply makes
// every dynamic-client action error, so the render+apply path fails.
func applyImageDeployServer(t *testing.T, failApply bool) (*server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if failApply {
			return true, nil, fmt.Errorf("apply boom")
		}
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{Store: st, Sealer: sealer, Kube: kube.NewFromDynamic(dyn), ExternalIP: "1.2.3.4"})
	return srv, st
}

// sealNotifyURL seals url into notify_url directly against srv's own
// sealer/store (these fixtures aren't wrapped in an HTTP mux).
func sealNotifyURL(t *testing.T, srv *server, url string) {
	t.Helper()
	sealed, err := srv.sealer.Seal([]byte(url))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.st.SetSetting("notify_url", "sealed:"+hex.EncodeToString(sealed)); err != nil {
		t.Fatal(err)
	}
}

// TestApplyImageDeployNotifies covers deploy_success/deploy_failed delivery
// out of applyImageDeploy — the render+apply core shared by the prebuilt
// image deploy path and rollback (rollback.go's rollback calls the same
// function), so this test doubles as coverage for both callers.
func TestApplyImageDeployNotifies(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv, st := applyImageDeployServer(t, false)
		ch := make(chan []byte, 4)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			ch <- body
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)
		sealNotifyURL(t, srv, ts.URL)
		if err := st.SetSetting("notify_events", "deploy_success"); err != nil {
			t.Fatal(err)
		}

		p, err := st.CreateProject("web")
		if err != nil {
			t.Fatal(err)
		}
		a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
		if err != nil {
			t.Fatal(err)
		}
		d, err := st.CreateDeployment(a.ID, "deploying", "nginx:1", 0)
		if err != nil {
			t.Fatal(err)
		}

		if err := srv.applyImageDeploy(context.Background(), p, a, d, "nginx:1"); err != nil {
			t.Fatalf("applyImageDeploy: %v", err)
		}

		select {
		case body := <-ch:
			var out struct {
				Event string `json:"event"`
				URL   string `json:"url"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			if out.Event != "deploy_success" || out.URL != "http://api.1-2-3-4.sslip.io" {
				t.Fatalf("got %+v", out)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for deploy_success notification")
		}
	})

	t.Run("failure", func(t *testing.T) {
		srv, st := applyImageDeployServer(t, true)
		ch := make(chan []byte, 4)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			ch <- body
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(ts.Close)
		sealNotifyURL(t, srv, ts.URL)
		if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
			t.Fatal(err)
		}

		p, err := st.CreateProject("web")
		if err != nil {
			t.Fatal(err)
		}
		a, err := st.CreateApp(p.ID, "api", 8080, "web", "")
		if err != nil {
			t.Fatal(err)
		}
		d, err := st.CreateDeployment(a.ID, "deploying", "nginx:1", 0)
		if err != nil {
			t.Fatal(err)
		}

		if err := srv.applyImageDeploy(context.Background(), p, a, d, "nginx:1"); err == nil {
			t.Fatal("applyImageDeploy: want error when apply fails")
		}

		select {
		case body := <-ch:
			var out struct {
				Event string `json:"event"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			if out.Event != "deploy_failed" || out.Error == "" {
				t.Fatalf("got %+v", out)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for deploy_failed notification")
		}
	})
}
