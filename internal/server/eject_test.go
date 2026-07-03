package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

// ejectTestServer builds a kube-backed, DataDir-enabled test server — same
// fixture shape as addonTestServer/rollbackServer (podMetricsGVR registered
// so the metrics read-path doesn't panic), plus a temp DataDir so the eject
// endpoint has somewhere to save the archived YAML.
func ejectTestServer(t *testing.T) (*httptestServer, *store.Store, *[]string, string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"})
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		if a.GetVerb() == "get" || a.GetVerb() == "list" {
			return false, nil, nil
		}
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	srv := newHTTPTest(t, Deps{
		Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer,
		ExternalIP: "1.2.3.4", DataDir: dataDir,
	})
	return srv, st, &actions, dataDir
}

// assertAppEjected fails t unless resp is a 409 with the app_ejected error code.
func assertAppEjected(t *testing.T, resp *http.Response, label string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("%s: want 409, got %d", label, resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("%s: decode: %v", label, err)
	}
	if env.Error.Code != "app_ejected" {
		t.Fatalf("%s: code = %q, want app_ejected", label, env.Error.Code)
	}
}

func TestEjectFlow(t *testing.T) {
	srv, st, actions, dataDir := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	// An addon to attach after eject, for the guard-matrix check below.
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"redis"}`).Body.Close()

	// 1. Eject.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eject: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		YAML    string `json:"yaml"`
		SavedTo string `json:"saved_to"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(out.YAML, "kind: Deployment") {
		t.Fatalf("eject yaml missing Deployment: %s", out.YAML)
	}
	wantSaved := filepath.Join(dataDir, "ejected", "proj-web.yaml")
	if out.SavedTo != wantSaved {
		t.Fatalf("saved_to = %q, want %q", out.SavedTo, wantSaved)
	}
	if _, err := os.Stat(wantSaved); err != nil {
		t.Fatalf("archived yaml file: %v", err)
	}

	// 2. Second eject is refused.
	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, ""), "second eject")

	// 3. Guard matrix — every mutation is refused.
	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/deploy", admin, `{"image":"nginx:2"}`), "deploy")
	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/scale", admin, `{"replicas":2}`), "scale")
	assertAppEjected(t, doAuthed(t, "PUT", srv.URL+"/v1/projects/proj/apps/web/env", admin, `{"key":"X","value":"1"}`), "set env")
	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/domains", admin, `{"hostname":"foo.example.com"}`), "add domain")
	assertAppEjected(t, doAuthed(t, "PUT", srv.URL+"/v1/projects/proj/apps/web/overrides/Deployment", admin, `{}`), "set override")
	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/rollback", admin, `{}`), "rollback")
	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/redis1/attach", admin, `{"app":"web"}`), "attach addon")

	// 4. Reads still work.
	for _, path := range []string{
		"/v1/projects/proj/apps/web",
		"/v1/projects/proj/apps/web/raw",
		"/v1/projects/proj/apps/web/metrics",
	} {
		resp := doAuthed(t, "GET", srv.URL+path, admin, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// 5. Destroy skips kube object deletion but removes the DB row.
	*actions = nil
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/apps/web", admin, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("destroy: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	joined := strings.Join(*actions, ",")
	if strings.Contains(joined, "delete") {
		t.Fatalf("destroy of an ejected app should not touch kube: %s", joined)
	}
	if _, err := st.GetApp(mustProjectID(t, st, "proj"), "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("app row should be gone: %v", err)
	}
}

func TestPushRefusesEjected(t *testing.T) {
	srv, st, _ := buildServer(t)

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080)
	if err != nil {
		t.Fatal(err)
	}

	u, err := st.CreateUser("dev@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(p.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	if err := st.SetAppEjected(a.ID); err != nil {
		t.Fatal(err)
	}

	backend := &PushBackend{s: srv}
	if _, err := backend.Branch(u, "web", "api"); err == nil || !strings.Contains(err.Error(), "ejected") {
		t.Fatalf("Branch on ejected app = %v, want error containing \"ejected\"", err)
	}
}
