package server

import (
	"encoding/json"
	"errors"
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

// addonTestServer builds a *server (for calling renderApp directly) plus an
// HTTP frontend over the same instance, both backed by a fake dynamic
// client that records every action — same fixture shape as apps_test.go's
// kubeServer and certs_test.go's certTestServer.
func addonTestServer(t *testing.T) (*server, *httptest.Server, *store.Store, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"})
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		if a.GetVerb() == "get" || a.GetVerb() == "list" {
			// Let the default tracker answer reads (StatefulSetReady/the app
			// page's metrics stats line need a real result, not a swallowed
			// nil object).
			return false, nil, nil
		}
		return true, nil, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer, ExternalIP: "1.2.3.4"})
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return s, srv, st, &actions
}

func TestAddonCreateAttachInject(t *testing.T) {
	s, srv, st, actions := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create + attach in one call.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"postgres","app":"web"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create addon: want 201, got %d", resp.StatusCode)
	}
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created["name"] != "postgres1" {
		t.Fatalf("addon name = %v", created["name"])
	}
	if created["status"] != "provisioning" {
		t.Fatalf("addon status = %v", created["status"])
	}
	joined := strings.Join(*actions, ",")
	if !strings.Contains(joined, "patch statefulsets") || !strings.Contains(joined, "patch secrets") {
		t.Fatalf("kube actions missing statefulset/secret apply: %s", joined)
	}

	// 2. List: attached_to ["web"].
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/addons", admin, "")
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}
	attached, _ := list[0]["attached_to"].([]any)
	if len(attached) != 1 || attached[0] != "web" {
		t.Fatalf("attached_to = %v", list[0]["attached_to"])
	}

	// 3. renderApp injects DATABASE_URL for the attached addon.
	secretJSON := func(t *testing.T) string {
		t.Helper()
		rendered, err := s.renderApp(p, a, "nginx:1", true)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range rendered.Objects {
			if o.Kind == "Secret" {
				return string(o.JSON)
			}
		}
		t.Fatal("no Secret object rendered")
		return ""
	}
	sec := secretJSON(t)
	// postgresql:// scheme, not postgres:// — SQLAlchemy rejects the short form.
	if !strings.Contains(sec, "postgresql://") || !strings.Contains(sec, "addon-postgres1."+p.Namespace) || !strings.Contains(sec, ":5432/app") {
		t.Fatalf("secret missing injected DATABASE_URL: %s", sec)
	}

	// 4. User env wins collisions; duplicate attach is rejected.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/proj/apps/web/env", admin, `{"key":"DATABASE_URL","value":"custom"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set env: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	sec = secretJSON(t)
	if !strings.Contains(sec, `"DATABASE_URL":"custom"`) {
		t.Fatalf("user env should win collision: %s", sec)
	}

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/postgres1/attach", admin, `{"app":"web"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate attach: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Detach removes the injection; the user's own DATABASE_URL remains.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/postgres1/detach", admin, `{"app":"web"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("detach: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	sec = secretJSON(t)
	if !strings.Contains(sec, `"DATABASE_URL":"custom"`) {
		t.Fatalf("user env should survive detach: %s", sec)
	}
}

func TestAddonRemoveGuard(t *testing.T) {
	_, srv, st, actions := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"redis","app":"web"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create addon: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Attached, no force: 409.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/addons/redis1", admin, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete without force: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Force: 204, and the cluster objects are deleted.
	*actions = nil
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/proj/addons/redis1?force=1", admin, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete with force: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	joined := strings.Join(*actions, ",")
	if !strings.Contains(joined, "delete statefulsets") {
		t.Fatalf("no statefulset delete recorded: %s", joined)
	}

	if _, err := st.GetAddon(mustProjectID(t, st, "proj"), "redis1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("addon row should be gone: %v", err)
	}
}

func TestAddonSecondSameTypeSuffix(t *testing.T) {
	s, srv, st, _ := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"postgres","app":"web"}`).Body.Close()
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"postgres","app":"web"}`)
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created["name"] != "postgres2" {
		t.Fatalf("second addon name = %v", created["name"])
	}

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := s.renderApp(p, a, "nginx:1", true)
	if err != nil {
		t.Fatal(err)
	}
	var sec string
	for _, o := range rendered.Objects {
		if o.Kind == "Secret" {
			sec = string(o.JSON)
		}
	}
	if !strings.Contains(sec, `"DATABASE_URL":`) || !strings.Contains(sec, `"DATABASE_URL_POSTGRES2":`) {
		t.Fatalf("expected suffixed second addon key: %s", sec)
	}
}

func TestAddonUpgrade(t *testing.T) {
	_, srv, st, actions := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin,
		`{"type":"postgres","name":"pg1"}`).Body.Close()

	*actions = nil
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/pg1/upgrade", admin,
		`{"version":"17"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("upgrade: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Warning string `json:"warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Version != "17" {
		t.Fatalf("version = %q, want 17", out.Version)
	}
	if !strings.Contains(out.Warning, "take a backup") {
		t.Fatalf("warning = %q, want migration warning", out.Warning)
	}

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAddon(p.ID, "pg1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "17" {
		t.Fatalf("stored version = %q, want 17", a.Version)
	}

	applied := strings.Join(*actions, ",")
	if !strings.Contains(applied, "statefulsets") {
		t.Fatalf("no StatefulSet apply recorded, actions: %s", applied)
	}

	// Missing addon -> 404.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/nope/upgrade", admin,
		`{"version":"17"}`)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing addon: want 404, got %d", resp.StatusCode)
	}

	// Empty version -> 400.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/pg1/upgrade", admin, `{}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("empty version: want 400, got %d", resp.StatusCode)
	}
}
