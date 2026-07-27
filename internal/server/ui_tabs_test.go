package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestUITabDefaultRendersOverview checks the app page's default (no ?tab=)
// load lands on Overview: the tab bar is present, and so are Overview's two
// cards — the status board and the literal deploy pipeline (DESIGN.md
// UX Architecture v3).
func TestUITabDefaultRendersOverview(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	// A shipped app: the never-deployed Launch Sequence checklist (DESIGN.md
	// "Launch Sequence") replaces the status board + pipeline this test
	// otherwise asserts on.
	if _, err := st.CreateDeployment(appID(t, st, "proj", "web"), "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	for _, want := range []string{
		`class="tab-link active"`, ">Overview<", ">Ship<", ">Observe<", ">Wire<", ">Data<",
		"<h2>Status</h2>", "<h2>Deploy pipeline</h2>", "luncur status web",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("default app page missing %q, got:\n%s", want, body)
		}
	}
	// Overview is the only active tab; Wire-only content must not leak in.
	if strings.Contains(body, "<h2>Environment</h2>") {
		t.Fatalf("default (Overview) page should not render Wire's Environment card, got:\n%s", body)
	}
}

// TestUITabWireShowsEnvAndDestructiveDisclosure checks ?tab=wire's full page
// contains the Environment card and the bottom "-- destructive --"
// disclosure (Danger zone is not its own tab, DESIGN.md UX Architecture v3).
func TestUITabWireShowsEnvAndDestructiveDisclosure(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web?tab=wire", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page (wire): want 200, got %d", status)
	}
	if !strings.Contains(body, "<h2>Environment</h2>") {
		t.Fatalf("wire tab missing Environment card, got:\n%s", body)
	}
	if !strings.Contains(body, "-- destructive --") {
		t.Fatalf("wire tab missing destructive disclosure, got:\n%s", body)
	}
	if !strings.Contains(body, "<h2>Danger zone</h2>") {
		t.Fatalf("wire tab's destructive disclosure missing Danger zone, got:\n%s", body)
	}
}

// TestUITabHXRequestReturnsFragmentWithoutSidebar checks a tab-switch htmx
// request (HX-Request set, HX-Boosted absent — matching the tab bar's own
// hx-get/hx-target, see app.html's "app_shell") gets back only the target
// tab's fragment: it must contain that tab's content but never the sidebar
// (`<aside`) or the other tabs' bar markup a full page carries.
func TestUITabHXRequestReturnsFragmentWithoutSidebar(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj/apps/web?tab=data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET app page (hx fragment): want 200, got %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if strings.Contains(body, "<aside") {
		t.Fatalf("hx fragment must not carry the sidebar, got:\n%s", body)
	}
	if strings.Contains(body, `class="tab-link`) {
		t.Fatalf("hx fragment must not carry the tab bar, got:\n%s", body)
	}
	if !strings.Contains(body, "<h2>Addons</h2>") {
		t.Fatalf("hx fragment for ?tab=data missing Addons card, got:\n%s", body)
	}
}

// TestUITabInvalidFallsBackToOverview checks an unknown ?tab= value renders
// the same content as the default (Overview) tab rather than erroring or
// rendering nothing.
func TestUITabInvalidFallsBackToOverview(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	// A shipped app: the never-deployed Launch Sequence checklist (DESIGN.md
	// "Launch Sequence") replaces the pipeline card this test asserts on.
	if _, err := st.CreateDeployment(appID(t, st, "proj", "web"), "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web?tab=bogus", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page (invalid tab): want 200, got %d", status)
	}
	if !strings.Contains(body, "<h2>Deploy pipeline</h2>") {
		t.Fatalf("invalid tab should fall back to Overview, got:\n%s", body)
	}
	if !strings.Contains(body, `class="tab-link active"`) {
		t.Fatalf("invalid tab should still mark a tab active (Overview), got:\n%s", body)
	}
}

// TestUIJobsTabVisibilityByKind checks the Jobs tab is reachable (200) for a
// job-kind app and absent from the tab bar for a plain web app — kind-gated
// per DESIGN.md UX Architecture v3 ("Jobs ... hidden when the app kind has
// none").
func TestUIJobsTabVisibilityByKind(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()
	// A shipped app: the never-deployed Launch Sequence checklist (DESIGN.md
	// "Launch Sequence") replaces the pipeline card this test asserts on.
	if _, err := st.CreateDeployment(appID(t, st, "proj", "web"), "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, jobBody := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/train?tab=jobs", ck)
	if status != http.StatusOK {
		t.Fatalf("GET job app page (jobs tab): want 200, got %d", status)
	}
	if !strings.Contains(jobBody, ">Jobs<") {
		t.Fatalf("job app's tab bar should include Jobs, got:\n%s", jobBody)
	}
	if !strings.Contains(jobBody, "<h2>Runs</h2>") {
		t.Fatalf("job app's jobs tab missing Runs card, got:\n%s", jobBody)
	}

	status, webBody := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("GET web app page: want 200, got %d", status)
	}
	if strings.Contains(webBody, ">Jobs<") {
		t.Fatalf("web app's tab bar should not include Jobs, got:\n%s", webBody)
	}

	// A direct ?tab=jobs request on a web app falls back to Overview instead
	// of erroring — the same "invalid for this app" fallback as an unknown
	// tab name.
	status, webJobsBody := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web?tab=jobs", ck)
	if status != http.StatusOK {
		t.Fatalf("GET web app page (?tab=jobs): want 200, got %d", status)
	}
	if !strings.Contains(webJobsBody, "<h2>Deploy pipeline</h2>") {
		t.Fatalf("web app's ?tab=jobs should fall back to Overview, got:\n%s", webJobsBody)
	}
}
