package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestUICreateAppWithImageDeploys checks the one-click "create app + deploy
// from image" path: posting an image alongside the create form creates the
// app AND kicks off a synchronous image deploy via the same applyImageDeploy
// core deployImage/rollback use, leaving a "live" deployment row for the new
// app with that image ref.
func TestUICreateAppWithImageDeploys(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	form := url.Values{
		"name": {"web"}, "kind": {"web"}, "port": {"80"},
		"image": {"nginx:1.27-alpine"},
	}
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps", csrfCk, ck, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST create with image: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj/apps/web" {
		t.Fatalf("POST create with image: want Location /ui/projects/proj/apps/web, got %q", loc)
	}

	id := appID(t, st, "proj", "web")
	d, err := st.LatestDeployment(id)
	if err != nil {
		t.Fatalf("expected a deployment row for the new app, got err=%v", err)
	}
	if d.ImageRef != "nginx:1.27-alpine" {
		t.Fatalf("deployment image ref: want nginx:1.27-alpine, got %q", d.ImageRef)
	}
	if d.Status != "live" {
		t.Fatalf("deployment status: want live, got %q", d.Status)
	}
}

// TestUICreateAppWithoutImageUnchanged checks the plain create-app path (no
// image field) behaves exactly as before: app created, no deployment row,
// redirect to the project page.
func TestUICreateAppWithoutImageUnchanged(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	form := url.Values{"name": {"web"}, "kind": {"web"}, "port": {"80"}}
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps", csrfCk, ck, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST create without image: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj" {
		t.Fatalf("POST create without image: want Location /ui/projects/proj, got %q", loc)
	}

	id := appID(t, st, "proj", "web")
	if _, err := st.LatestDeployment(id); err == nil {
		t.Fatal("create without image should not create a deployment row")
	}
}

// TestUICreateAppWithImageNoKube checks that when kube isn't configured, the
// app is still created (only the deploy attempt fails) and the redirect
// carries an ?err= the app page surfaces as a banner.
func TestUICreateAppWithImageNoKube(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	form := url.Values{
		"name": {"web"}, "kind": {"web"}, "port": {"80"},
		"image": {"nginx:1.27-alpine"},
	}
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps", csrfCk, ck, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST create with image, no kube: want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Fatalf("POST create with image, no kube: want Location to contain err=, got %q", loc)
	}
	if !strings.HasPrefix(loc, "/ui/projects/proj/apps/web") {
		t.Fatalf("POST create with image, no kube: want Location on the app page, got %q", loc)
	}

	// App itself must still exist.
	if _, err := st.GetApp(mustProjectID(t, st, "proj"), "web"); err != nil {
		t.Fatalf("app should still be created even though the deploy failed: %v", err)
	}
}
