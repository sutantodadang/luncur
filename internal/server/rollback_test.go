package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// fakeRegistry stands in for the embedded registry: it 200s HEAD requests
// for the given "name:tag" manifests and 404s everything else, mirroring
// the /v2/<name>/manifests/<tag> shape imageInRegistry probes. Returns the
// host:port to use as Deps.RegistryHost (the "http://" prefix stripped).
func fakeRegistry(t *testing.T, known ...string) string {
	t.Helper()
	knownSet := make(map[string]bool, len(known))
	for _, k := range known {
		knownSet[k] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/")
		idx := strings.LastIndex(p, "/manifests/")
		if idx < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		name, tag := p[:idx], p[idx+len("/manifests/"):]
		if knownSet[name+":"+tag] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// rollbackServer builds a kube-backed test server (fake dynamic client, apply
// always succeeds) whose Deps.RegistryHost points at registryHost.
func rollbackServer(t *testing.T, registryHost string) (*httptestServer, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	// PodMetrics needs a registered list kind — the app page's metrics
	// stats line calls kube.AppMetrics, which the fake dynamic client
	// panics on for any GVR it can't resolve a ListKind for.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"})
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == "get" || a.GetVerb() == "list" {
			// Let the default tracker answer reads (the app page's metrics
			// stats line needs a real List/Get result, not a swallowed nil).
			return false, nil, nil
		}
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{
		Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer,
		ExternalIP: "1.2.3.4", RegistryHost: registryHost,
	})
	return srv, st
}

func TestRollbackHappyPath(t *testing.T) {
	registryHost := fakeRegistry(t, "proj/web:1", "proj/web:2")
	srv, st := rollbackServer(t, registryHost)

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	d1, err := st.CreateDeployment(id, "live", registryHost+"/proj/web:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(id, "live", registryHost+"/proj/web:2", 0); err != nil {
		t.Fatal(err)
	}

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/rollback", admin, `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out struct {
		DeploymentID string `json:"deployment_id"`
		Seq          int64  `json:"seq"`
		Status       string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// deployment_id is an opaque nanoid now — assert its shape and the
	// human-facing seq, not a literal id value.
	if !validDeployID(out.DeploymentID) {
		t.Fatalf("deployment_id %q is not a valid nanoid", out.DeploymentID)
	}
	if out.Seq != 3 {
		t.Fatalf("seq = %d, want 3", out.Seq)
	}
	if out.Status != "live" {
		t.Fatalf("status = %q, want live", out.Status)
	}

	got, err := st.GetDeployment(out.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Fatalf("row status = %q, want live", got.Status)
	}
	if got.ImageRef != registryHost+"/proj/web:1" {
		t.Fatalf("row image = %q, want %s", got.ImageRef, registryHost+"/proj/web:1")
	}
	if got.RolledBackFrom != d1.ID {
		t.Fatalf("rolled_back_from = %q, want %q", got.RolledBackFrom, d1.ID)
	}
}

func TestRollbackExplicitAndErrors(t *testing.T) {
	registryHost := fakeRegistry(t, "proj/web:1", "proj/web:2")
	srv, st := rollbackServer(t, registryHost)

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web2","port":8081}`).Body.Close()

	webID := appID(t, st, "proj", "web")
	d1, err := st.CreateDeployment(webID, "live", registryHost+"/proj/web:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(webID, "live", registryHost+"/proj/web:2", 0); err != nil {
		t.Fatal(err)
	}
	// Unknown to the fake registry: exercises the 409 image_missing path.
	dMissing, err := st.CreateDeployment(webID, "live", registryHost+"/proj/web:9", 0)
	if err != nil {
		t.Fatal(err)
	}

	web2ID := appID(t, st, "proj", "web2")
	if _, err := st.CreateDeployment(web2ID, "live", registryHost+"/proj/web2:1", 0); err != nil {
		t.Fatal(err)
	}

	// Explicit deploy_id → 202, rolls back to tag :1.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/rollback", admin,
		fmt.Sprintf(`{"deploy_id":%q}`, d1.ID))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("explicit: want 202, got %d", resp.StatusCode)
	}
	var out struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got, err := st.GetDeployment(out.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != registryHost+"/proj/web:1" {
		t.Fatalf("explicit rollback image = %q, want :1", got.ImageRef)
	}

	// Nonexistent deploy id (valid nanoid shape, no such row) → 404 not_found.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/rollback", admin, `{"deploy_id":"999999999999"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad id: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Registry doesn't know the tag → 409 image_missing, message names the image.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/rollback", admin,
		fmt.Sprintf(`{"deploy_id":%q}`, dMissing.ID))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("missing image: want 409, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if env.Error.Code != "image_missing" {
		t.Fatalf("code = %q, want image_missing", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, registryHost+"/proj/web:9") {
		t.Fatalf("message %q missing image ref", env.Error.Message)
	}

	// Auto-pick with only one live deployment → 409 no_target.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web2/rollback", admin, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("no target: want 409, got %d", resp.StatusCode)
	}
	var env2 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env2); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if env2.Error.Code != "no_target" {
		t.Fatalf("code = %q, want no_target", env2.Error.Code)
	}
}
