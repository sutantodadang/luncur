package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestUIProjectCreate checks the create-project route is admin-gated (a
// member's POST 404s, matching uiAdmin's leak-nothing policy) and that a
// valid admin POST creates the project and redirects to /ui/, while a
// duplicate name redirects back with an ?err= banner instead of erroring
// out.
func TestUIProjectCreate(t *testing.T) {
	srv, st := testServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	memberResp := uiPost(t, client, srv.URL+"/ui/projects", csrfCk, uiSessionCookie(t, st, member.ID),
		url.Values{"name": {"web"}})
	memberResp.Body.Close()
	if memberResp.StatusCode != http.StatusNotFound {
		t.Fatalf("member create project: want 404, got %d", memberResp.StatusCode)
	}

	adminCk := uiSessionCookie(t, st, admin.ID)
	resp := uiPost(t, client, srv.URL+"/ui/projects", csrfCk, adminCk, url.Values{"name": {"web"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin create project: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("Location = %q, want /ui/", loc)
	}
	if _, err := st.GetProject("web"); err != nil {
		t.Fatalf("GetProject after create: %v", err)
	}

	dup := uiPost(t, client, srv.URL+"/ui/projects", csrfCk, adminCk, url.Values{"name": {"web"}})
	dup.Body.Close()
	if dup.StatusCode != http.StatusSeeOther {
		t.Fatalf("duplicate project: want 303, got %d", dup.StatusCode)
	}
	if loc := dup.Header.Get("Location"); !strings.HasPrefix(loc, "/ui/?err=") {
		t.Fatalf("Location = %q, want an error redirect", loc)
	}
}

// TestUIAddMember checks the add-member route is admin-gated, a valid POST
// adds the member and redirects to the project page, and an unknown email
// redirects back with an ?err= banner instead of a hard error.
func TestUIAddMember(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()

	adminUser, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	memberResp := uiPost(t, client, srv.URL+"/ui/projects/proj/members", csrfCk, uiSessionCookie(t, st, member.ID),
		url.Values{"email": {"m@b.co"}})
	memberResp.Body.Close()
	if memberResp.StatusCode != http.StatusNotFound {
		t.Fatalf("member add-member: want 404, got %d", memberResp.StatusCode)
	}

	adminCk := uiSessionCookie(t, st, adminUser.ID)
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/members", csrfCk, adminCk, url.Values{"email": {"m@b.co"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin add member: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj" {
		t.Fatalf("Location = %q, want /ui/projects/proj", loc)
	}
	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.IsMember(p.ID, member.ID); !ok {
		t.Fatal("want member added")
	}

	ghost := uiPost(t, client, srv.URL+"/ui/projects/proj/members", csrfCk, adminCk, url.Values{"email": {"ghost@b.co"}})
	ghost.Body.Close()
	if ghost.StatusCode != http.StatusSeeOther {
		t.Fatalf("unknown email: want 303, got %d", ghost.StatusCode)
	}
	if loc := ghost.Header.Get("Location"); !strings.HasPrefix(loc, "/ui/projects/proj?err=") {
		t.Fatalf("Location = %q, want an error redirect", loc)
	}
}

// TestUIAppDestroy exercises the destroy route end to end: kube objects are
// deleted (recorded on the fake dynamic client) and the app row is gone from
// the store, with a redirect back to the project page.
func TestUIAppDestroy(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	*actions = nil
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/destroy", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("destroy app: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj" {
		t.Fatalf("Location = %q, want /ui/projects/proj", loc)
	}

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetApp(p.ID, "web"); err == nil {
		t.Fatal("app should be gone after destroy")
	}
	if len(*actions) == 0 {
		t.Fatal("expected kube delete actions to be recorded")
	}
}

// TestUIEject checks the eject route marks the app ejected in the store and
// redirects back to the app page.
func TestUIEject(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/eject", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("eject: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj/apps/web" {
		t.Fatalf("Location = %q, want the app page", loc)
	}

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Ejected {
		t.Fatal("app should be ejected")
	}

	// A second eject on an already-ejected app is a 409, matching
	// handleEjectApp's refuseEjected guard.
	again := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/eject", csrfCk, ck, url.Values{})
	again.Body.Close()
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("re-eject: want 409, got %d", again.StatusCode)
	}
}

// TestUIDomainRetry seeds a domain with a failed cert status directly in the
// store, then checks the retry route resets it to "none" and redirects back
// to the app page.
func TestUIDomainRetry(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	dom, err := st.AddDomain(id, "www.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDomainCert(dom.ID, "failed", "boom", ""); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/domains/retry", csrfCk, ck,
		url.Values{"hostname": {"www.example.com"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("domain retry: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj/apps/web" {
		t.Fatalf("Location = %q, want the app page", loc)
	}

	list, err := st.ListDomains(id)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDomains = %+v, err=%v", list, err)
	}
	if list[0].CertStatus != "none" {
		t.Fatalf("CertStatus = %q, want none after retry", list[0].CertStatus)
	}

	// Unknown hostname -> 404.
	missing := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/domains/retry", csrfCk, ck,
		url.Values{"hostname": {"nope.example.com"}})
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown hostname: want 404, got %d", missing.StatusCode)
	}
}

// TestUIAddonUpgrade checks the upgrade route persists the new version,
// applies the re-rendered manifests (recorded on the fake dynamic client),
// and redirects back to the project page.
func TestUIAddonUpgrade(t *testing.T) {
	_, srv, st, actions := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"postgres","name":"pg1"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	*actions = nil
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/addons/upgrade", csrfCk, ck,
		url.Values{"name": {"pg1"}, "version": {"17"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("addon upgrade: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj" {
		t.Fatalf("Location = %q, want /ui/projects/proj", loc)
	}

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	ad, err := st.GetAddon(p.ID, "pg1")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Version != "17" {
		t.Fatalf("stored version = %q, want 17", ad.Version)
	}
	if !strings.Contains(strings.Join(*actions, ","), "statefulsets") {
		t.Fatalf("no StatefulSet apply recorded, actions: %v", *actions)
	}

	// Missing version -> 400.
	badReq := uiPost(t, client, srv.URL+"/ui/projects/proj/addons/upgrade", csrfCk, ck, url.Values{"name": {"pg1"}})
	badReq.Body.Close()
	if badReq.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing version: want 400, got %d", badReq.StatusCode)
	}
}

// TestUIAddonURL checks the on-demand connection-reveal fragment: 200 with
// the KEY=URL body for a logged-in session, 303 (login redirect) without
// one, and 404 for an unknown addon name.
func TestUIAddonURL(t *testing.T) {
	_, srv, st, _ := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin, `{"type":"postgres","name":"postgres1"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/addons/url?name=postgres1", ck)
	if status != http.StatusOK {
		t.Fatalf("addon url fragment: want 200, got %d", status)
	}
	if !strings.Contains(body, "DATABASE_URL=postgresql://") || !strings.Contains(body, "readonly") {
		t.Fatalf("fragment missing expected content: %s", body)
	}

	status, _ = getUIPage(t, client, srv.URL, "/ui/projects/proj/addons/url?name=postgres1", nil)
	if status != http.StatusSeeOther {
		t.Fatalf("no session: want 303, got %d", status)
	}

	status, _ = getUIPage(t, client, srv.URL, "/ui/projects/proj/addons/url?name=nope", ck)
	if status != http.StatusNotFound {
		t.Fatalf("unknown addon: want 404, got %d", status)
	}
}

// TestUIProjectsMembersPageFields checks the projects/apps pages render the
// new create-project and add-member forms for an admin, and hide the write
// forms (but still show the list) for a plain member.
func TestUIProjectsMembersPageFields(t *testing.T) {
	srv, st := testServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)

	if _, err := st.CreateProject("proj"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(mustProjectID(t, st, "proj"), member.ID, "member"); err != nil {
		t.Fatal(err)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/", adminCk)
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/: want 200, got %d", status)
	}
	if !strings.Contains(body, `action="/ui/projects"`) {
		t.Fatalf("admin projects page missing create-project form, got: %s", body)
	}

	memberCk := uiSessionCookie(t, st, member.ID)
	status, body = getUIPage(t, client, srv.URL, "/ui/", memberCk)
	if status != http.StatusOK {
		t.Fatalf("member GET /ui/: want 200, got %d", status)
	}
	if strings.Contains(body, `action="/ui/projects"`) {
		t.Fatalf("member projects page should not show create-project form, got: %s", body)
	}

	status, body = getUIPage(t, client, srv.URL, "/ui/projects/proj", adminCk)
	if status != http.StatusOK {
		t.Fatalf("admin GET project page: want 200, got %d", status)
	}
	if !strings.Contains(body, "m@b.co") {
		t.Fatalf("project page missing member list, got: %s", body)
	}
	if !strings.Contains(body, `action="/ui/projects/proj/members"`) {
		t.Fatalf("admin project page missing add-member form, got: %s", body)
	}

	status, body = getUIPage(t, client, srv.URL, "/ui/projects/proj", memberCk)
	if status != http.StatusOK {
		t.Fatalf("member GET project page: want 200, got %d", status)
	}
	if strings.Contains(body, `action="/ui/projects/proj/members"`) {
		t.Fatalf("member project page should not show add-member form, got: %s", body)
	}
}
