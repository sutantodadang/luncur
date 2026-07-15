package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

func TestRenameProjectAPI(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "member@b.co", "member")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"taken"}`).Body.Close()

	// Non-admin is forbidden.
	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web", member, `{"name":"webapp"}`)
	if resp.StatusCode != 403 {
		t.Fatalf("member rename: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid name.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/web", admin, `{"name":"-bad"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid name: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Duplicate name.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/web", admin, `{"name":"taken"}`)
	if resp.StatusCode != 409 {
		t.Fatalf("dup name: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Success.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/web", admin, `{"name":"webapp"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("rename: want 200, got %d", resp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["name"] != "webapp" || got["namespace"] != "luncur-web" {
		t.Fatalf("renamed project: %+v", got)
	}
}

func TestDeleteProjectAPI(t *testing.T) {
	// Non-ejected app with no kube configured: 503.
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web", admin, "")
	if resp.StatusCode != 503 {
		t.Fatalf("delete without kube: want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A project with no apps/addons needs no kube at all.
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"empty"}`).Body.Close()
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/empty", admin, "")
	if resp.StatusCode != 204 {
		t.Fatalf("delete empty project: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// With kube configured (dynamic client + a fake core clientset, so
	// DeleteNamespace has a CoreV1 to call), a project with a live app
	// tears down cleanly.
	kst := newTestStore(t)
	kc := kube.NewForTest(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), k8sfake.NewSimpleClientset())
	ksrv := newHTTPTest(t, Deps{Store: kst, Kube: kc})
	kadmin := seedUserToken(t, kst, "root@b.co", "admin")
	doAuthed(t, "POST", ksrv.URL+"/v1/projects", kadmin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", ksrv.URL+"/v1/projects/web/apps", kadmin, `{"name":"api","port":3000}`).Body.Close()
	p, err := kst.GetProject("web")
	if err != nil {
		t.Fatal(err)
	}

	resp = doAuthed(t, "DELETE", ksrv.URL+"/v1/projects/web", kadmin, "")
	if resp.StatusCode != 204 {
		t.Fatalf("delete with kube: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if _, err := kst.GetProjectByID(p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project row should be gone, got %v", err)
	}
	apps, err := kst.ListApps(p.ID)
	if err != nil || len(apps) != 0 {
		t.Fatalf("app rows should be gone: %+v %v", apps, err)
	}
}

// TestDeleteProjectTearsDownAllEnvironmentNamespaces locks the fix that
// makes deleteProject walk EVERY environment (not just the default/
// production one, which used to be the only namespace it deleted): a
// project with an app and an addon living in a non-default (develop)
// environment must have both the production and the develop namespaces
// deleted, and the develop env's app/addon rows must not be orphaned.
func TestDeleteProjectTearsDownAllEnvironmentNamespaces(t *testing.T) {
	st := newTestStore(t)
	cs := k8sfake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, Kube: kube.NewForTest(dyn, cs)})

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	prod, err := st.GetEnvironment(p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetEnvironment(p.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Namespace == prod.Namespace {
		t.Fatalf("test fixture invalid: develop namespace %q equals production namespace", dev.Namespace)
	}

	ctx := context.Background()
	for _, ns := range []string{prod.Namespace, dev.Namespace} {
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// An app and an addon living in the NON-default develop environment.
	if _, err := st.CreateAppInEnv(dev.ID, "worker", 9090, "web", ""); err != nil {
		t.Fatal(err)
	}
	ad := seedPreviewAddon(t, s, st, dev, "postgres", "db1")

	if err := s.deleteProject(ctx, p); err != nil {
		t.Fatalf("deleteProject: %v", err)
	}

	// Both namespaces are gone — no leak of the non-default environment's
	// namespace.
	for _, ns := range []string{prod.Namespace, dev.Namespace} {
		if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); !kube.IsNotFound(err) {
			t.Fatalf("namespace %s should be deleted, got err=%v", ns, err)
		}
	}

	// The develop env's rows are gone too — nothing orphaned.
	if apps, err := st.ListAppsInEnv(dev.ID); err != nil || len(apps) != 0 {
		t.Fatalf("develop app rows still present: %+v, err %v", apps, err)
	}
	if addons, err := st.AddonsForEnv(dev.ID); err != nil || len(addons) != 0 {
		t.Fatalf("develop addon rows still present: %+v, err %v", addons, err)
	}
	allAddons, err := st.ListAddons(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range allAddons {
		if a.ID == ad.ID {
			t.Fatalf("addon %s row was not deleted", ad.Name)
		}
	}
	if _, err := st.GetProjectByID(p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project row should be gone, got %v", err)
	}
}

func TestRemoveMemberAPI(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "member@b.co", "member")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/members", admin, `{"email":"member@b.co"}`).Body.Close()

	// Member can see the project's apps before removal.
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps", member, "")
	if resp.StatusCode != 200 {
		t.Fatalf("member list apps before removal: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/members/member@b.co", admin, "")
	if resp.StatusCode != 204 {
		t.Fatalf("remove member: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps", member, "")
	if resp.StatusCode != 403 {
		t.Fatalf("member list apps after removal: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/members/member@b.co", admin, "")
	if resp.StatusCode != 404 {
		t.Fatalf("remove non-member: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestUIProjectRenameDelete exercises the project page's new "Project
// settings" card end to end: an admin renames the (app-less) project via
// POST (303 to the renamed project's page), then deletes it via POST (303
// to the project list), and the project is gone from the store afterward.
func TestUIProjectRenameDelete(t *testing.T) {
	srv, st := testServer(t) // no kube; project has no apps/addons
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	// The project page advertises the settings card and its CLI hints.
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/web", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	page, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	pageBytes, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	pageBody := string(pageBytes)
	if !strings.Contains(pageBody, "Project settings") || !strings.Contains(pageBody, "luncur project rm") {
		t.Fatalf("project page: want settings card, got: %s", pageBody)
	}

	csrfCk := uiCSRF(t, client, srv.URL)
	renameResp := uiPost(t, client, srv.URL+"/ui/projects/web/rename", csrfCk, ck, url.Values{"name": {"webapp"}})
	renameResp.Body.Close()
	if renameResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST rename: want 303, got %d", renameResp.StatusCode)
	}
	if loc := renameResp.Header.Get("Location"); loc != "/ui/projects/webapp" {
		t.Fatalf("POST rename: want Location /ui/projects/webapp, got %q", loc)
	}

	deleteResp := uiPost(t, client, srv.URL+"/ui/projects/webapp/delete", csrfCk, ck, url.Values{})
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST delete: want 303, got %d", deleteResp.StatusCode)
	}
	if loc := deleteResp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("POST delete: want Location /ui/, got %q", loc)
	}

	if _, err := st.GetProject("webapp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want project gone, got %v", err)
	}
}
