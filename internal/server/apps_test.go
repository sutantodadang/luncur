package server

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

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
