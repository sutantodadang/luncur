package server

import (
	"strings"
	"testing"
)

// TestUITreeShowsProjectEnvApp covers the workspace tree's basic shape
// (DESIGN.md "UX Architecture (v3)"): a member sees their own project's
// tree rendered project/ -> env/ -> app on the projects landing page.
func TestUITreeShowsProjectEnvApp(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "admin@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()

	p, err := st.GetProject("waku")
	if err != nil {
		t.Fatal(err)
	}
	_, env := seedDefaultEnv(t, st, p)
	if _, err := st.CreateAppInEnv(env.ID, "backend", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}

	member, err := st.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(p.ID, member.ID, "member"); err != nil {
		t.Fatal(err)
	}

	ck := uiSessionCookie(t, st, member.ID)
	status, body := getUIPage(t, noRedirectClient(), srv.URL, "/ui/", ck)
	if status != 200 {
		t.Fatalf("GET /ui/: want 200, got %d", status)
	}
	for _, want := range []string{
		`href="/ui/projects/waku"`,
		`href="/ui/projects/waku/envs/production"`,
		`href="/ui/projects/waku/envs/production/apps/backend"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET /ui/: tree missing %s, got: %s", want, body)
		}
	}
}

// TestUITreeHidesNonMemberProject is the tree's membership-scoping
// assertion (SECURITY): a member of one project must never see another
// project's rows in the sidebar, mirroring the same leak-nothing rule
// uiProject already enforces for direct navigation.
func TestUITreeHidesNonMemberProject(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "admin2@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"mine"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"theirs"}`).Body.Close()

	mine, err := st.GetProject("mine")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("scoped@b.co", "pw123456", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(mine.ID, member.ID, "member"); err != nil {
		t.Fatal(err)
	}

	ck := uiSessionCookie(t, st, member.ID)
	status, body := getUIPage(t, noRedirectClient(), srv.URL, "/ui/", ck)
	if status != 200 {
		t.Fatalf("GET /ui/: want 200, got %d", status)
	}
	if !strings.Contains(body, `href="/ui/projects/mine"`) {
		t.Fatalf("GET /ui/: tree missing member's own project, got: %s", body)
	}
	if strings.Contains(body, `href="/ui/projects/theirs"`) {
		t.Fatalf("GET /ui/: tree leaked non-member project, got: %s", body)
	}
}

// TestUITreeDotClassMatchesStatus covers the tree's status-dot mapping:
// no deployment yet -> muted, a live deployment -> ok, a failed one -> fail.
// Reuses the same status buckets as app.html's chip, not a parallel map.
func TestUITreeDotClassMatchesStatus(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "admin3@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	p, err := st.GetProject("waku")
	if err != nil {
		t.Fatal(err)
	}
	_, env := seedDefaultEnv(t, st, p)
	a, err := st.CreateAppInEnv(env.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("admin3@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	_, body := getUIPage(t, client, srv.URL, "/ui/", ck)
	if !strings.Contains(body, `tree-dot tree-dot-muted`) {
		t.Fatalf("GET /ui/: want muted dot for never-deployed app, got: %s", body)
	}

	if _, err := st.CreateDeployment(a.ID, "live", "img:1", 0); err != nil {
		t.Fatal(err)
	}
	_, body = getUIPage(t, client, srv.URL, "/ui/", ck)
	if !strings.Contains(body, `tree-dot tree-dot-ok`) {
		t.Fatalf("GET /ui/: want ok dot for live deployment, got: %s", body)
	}

	if _, err := st.CreateDeployment(a.ID, "failed", "img:2", 0); err != nil {
		t.Fatal(err)
	}
	_, body = getUIPage(t, client, srv.URL, "/ui/", ck)
	if !strings.Contains(body, `tree-dot tree-dot-fail`) {
		t.Fatalf("GET /ui/: want fail dot for failed deployment, got: %s", body)
	}
}

// TestUITreeActiveAppRow covers the active app's 2px left-border: only the
// app row matching the current page's URL carries the "active" class.
func TestUITreeActiveAppRow(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "admin4@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"waku"}`).Body.Close()
	p, err := st.GetProject("waku")
	if err != nil {
		t.Fatal(err)
	}
	_, env := seedDefaultEnv(t, st, p)
	if _, err := st.CreateAppInEnv(env.ID, "backend", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAppInEnv(env.ID, "worker", 0, "worker", ""); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("admin4@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	// On backend's own page, backend's row is active and worker's is not.
	_, body := getUIPage(t, client, srv.URL, "/ui/projects/waku/envs/production/apps/backend", ck)
	if !strings.Contains(body, `href="/ui/projects/waku/envs/production/apps/backend" class="tree-row tree-indent-2 tree-app active"`) {
		t.Fatalf("app page: want backend's tree row active, got: %s", body)
	}
	if strings.Contains(body, `href="/ui/projects/waku/envs/production/apps/worker" class="tree-row tree-indent-2 tree-app active"`) {
		t.Fatalf("app page: worker's tree row must not be active, got: %s", body)
	}

	// On an unrelated page, neither app row is active.
	_, body = getUIPage(t, client, srv.URL, "/ui/tokens", ck)
	if strings.Contains(body, `tree-app active`) {
		t.Fatalf("tokens page: no app row should be active, got: %s", body)
	}
}

// TestUITreeCapsAppsPerEnv covers the ponytail guard: an env with more than
// uiTreeMaxAppsPerEnv apps collapses the rest into a "+N more" link instead
// of listing every one, so a busy project can't blow up the sidebar.
func TestUITreeCapsAppsPerEnv(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "admin5@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"busy"}`).Body.Close()
	p, err := st.GetProject("busy")
	if err != nil {
		t.Fatal(err)
	}
	_, env := seedDefaultEnv(t, st, p)
	names := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"}
	for _, n := range names {
		if _, err := st.CreateAppInEnv(env.ID, n, 8080, "web", ""); err != nil {
			t.Fatal(err)
		}
	}

	u, err := st.GetUserByEmail("admin5@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	_, body := getUIPage(t, noRedirectClient(), srv.URL, "/ui/", ck)
	if !strings.Contains(body, "+1 more") {
		t.Fatalf("GET /ui/: want +1 more for a 9-app env, got: %s", body)
	}
	if !strings.Contains(body, `href="/ui/projects/busy" class="tree-row tree-indent-2 tree-more"`) {
		t.Fatalf("GET /ui/: want +N more link to project page, got: %s", body)
	}
}
