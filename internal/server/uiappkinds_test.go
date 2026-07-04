package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestUICreateAppFormPerKind exercises the project page's create-app form
// for all three kinds: web (default), worker (no port), and cron (schedule
// required).
func TestUICreateAppFormPerKind(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	sessionCk := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	// web app via the form.
	resp := uiPost(t, client, srv.URL+"/ui/projects/web/apps", csrfCk, sessionCk,
		url.Values{"name": {"api"}, "kind": {"web"}, "port": {"3000"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("web create: want 303, got %d", resp.StatusCode)
	}

	// worker app: no port needed.
	resp = uiPost(t, client, srv.URL+"/ui/projects/web/apps", csrfCk, sessionCk,
		url.Values{"name": {"worker1"}, "kind": {"worker"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("worker create: want 303, got %d", resp.StatusCode)
	}

	// cron app without schedule: rejected.
	resp = uiPost(t, client, srv.URL+"/ui/projects/web/apps", csrfCk, sessionCk,
		url.Values{"name": {"bad-cron"}, "kind": {"cron"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cron without schedule: want 400, got %d", resp.StatusCode)
	}

	// cron app with schedule: happy path.
	resp = uiPost(t, client, srv.URL+"/ui/projects/web/apps", csrfCk, sessionCk,
		url.Values{"name": {"nightly"}, "kind": {"cron"}, "schedule": {"0 3 * * *"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cron create: want 303, got %d", resp.StatusCode)
	}

	pID := mustProjectID(t, st, "web")
	for _, want := range []struct{ name, kind string }{
		{"api", "web"}, {"worker1", "worker"}, {"nightly", "cron"},
	} {
		a, err := st.GetApp(pID, want.name)
		if err != nil {
			t.Fatalf("app %s not created: %v", want.name, err)
		}
		if a.Kind != want.kind {
			t.Fatalf("app %s: kind=%q want %q", want.name, a.Kind, want.kind)
		}
	}
}

// TestUIAppPageSectionVisibilityPerKind checks the app detail page hides
// domains/health/replicas appropriately per kind: worker keeps replicas but
// loses health+domains; cron loses replicas+health+domains.
func TestUIAppPageSectionVisibilityPerKind(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"worker1","kind":"worker"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"nightly","kind":"cron","schedule":"0 3 * * *"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	get := func(path string) string {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(ck)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d", path, resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	worker := get("/ui/projects/web/apps/worker1")
	if !strings.Contains(worker, `name="replicas"`) {
		t.Fatal("worker page should keep the replicas input")
	}
	if strings.Contains(worker, "Health check") {
		t.Fatal("worker page should not show the health check section")
	}
	if strings.Contains(worker, "<h2>Domains</h2>") {
		t.Fatal("worker page should not show the domains section")
	}
	if !strings.Contains(worker, "edit/Deployment") || strings.Contains(worker, "edit/Service") || strings.Contains(worker, "edit/Ingress") {
		t.Fatalf("worker edit links wrong:\n%s", worker)
	}

	cron := get("/ui/projects/web/apps/nightly")
	if strings.Contains(cron, `name="replicas"`) {
		t.Fatal("cron page should not show the replicas input")
	}
	if strings.Contains(cron, "Health check") {
		t.Fatal("cron page should not show the health check section")
	}
	if strings.Contains(cron, "<h2>Domains</h2>") {
		t.Fatal("cron page should not show the domains section")
	}
	if !strings.Contains(cron, "edit/CronJob") {
		t.Fatal("cron page should link to the CronJob editor")
	}
	if !strings.Contains(cron, "0 3 * * *") {
		t.Fatal("cron page should show the schedule")
	}
}
