package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// getUIPage GETs path with the given session cookie and returns the status
// code and body. t.Fatal's on transport errors only — callers assert status.
func getUIPage(t *testing.T, client *http.Client, base, path string, ck *http.Cookie) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestUIChipFragment exercises the polling fragment: no session redirects,
// a building deploy carries the hx-trigger poll, and a live (terminal)
// deploy does not.
func TestUIChipFragment(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	id := appID(t, st, "proj", "web")

	client := noRedirectClient()
	chipPath := "/ui/projects/proj/apps/web/chip"

	// No session cookie -> redirect to login, not the fragment.
	status, _ := getUIPage(t, client, srv.URL, chipPath, nil)
	if status != http.StatusSeeOther {
		t.Fatalf("chip without session: want 303, got %d", status)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	if _, err := st.CreateDeployment(id, "building", "", 0); err != nil {
		t.Fatal(err)
	}
	status, body := getUIPage(t, client, srv.URL, chipPath, ck)
	if status != http.StatusOK {
		t.Fatalf("building chip: want 200, got %d", status)
	}
	if !strings.Contains(body, "hx-trigger") {
		t.Fatalf("building chip should carry hx-trigger, got: %s", body)
	}

	if _, err := st.CreateDeployment(id, "live", "img:v2", 0); err != nil {
		t.Fatal(err)
	}
	status, body = getUIPage(t, client, srv.URL, chipPath, ck)
	if status != http.StatusOK {
		t.Fatalf("live chip: want 200, got %d", status)
	}
	if strings.Contains(body, "hx-trigger") {
		t.Fatalf("live chip should not carry hx-trigger, got: %s", body)
	}
}

// TestUIDeployHistoryAndRollback checks the app page's Deploys card lists
// recent deployments and offers a rollback form per older row, matching
// handleUIRollback's deploy_id contract.
func TestUIDeployHistoryAndRollback(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	id := appID(t, st, "proj", "web")

	var ids []int64
	for _, img := range []string{"img:v1", "img:v2", "img:v3"} {
		d, err := st.CreateDeployment(id, "live", img, 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("app page: want 200, got %d", status)
	}
	for _, id := range ids {
		if !strings.Contains(body, `<td>`+strconv.FormatInt(id, 10)+`</td>`) {
			t.Fatalf("app page should list deploy id %d, got: %s", id, body)
		}
	}
	if !strings.Contains(body, `name="deploy_id"`) {
		t.Fatalf("app page should offer a rollback form with deploy_id, got: %s", body)
	}
	if !strings.Contains(body, "hx-confirm") {
		t.Fatalf("rollback button should carry hx-confirm, got: %s", body)
	}
	// Image column shows only the tag, full ref rides the title attribute.
	if !strings.Contains(body, `title="img:v3"`) || !strings.Contains(body, ">v3<") {
		t.Fatalf("image column should show tag v3 with full ref as title, got: %s", body)
	}
}

// TestUIAppsListStatusChip checks the project apps list shows a status chip
// for an app with a deploy, and a "no deploys" chip for a fresh app.
func TestUIAppsListStatusChip(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"deployed","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"fresh","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "deployed")
	if _, err := st.CreateDeployment(id, "live", "img:v1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj", ck)
	if status != http.StatusOK {
		t.Fatalf("apps page: want 200, got %d", status)
	}
	if !strings.Contains(body, `class="status-live"`) {
		t.Fatalf("apps page should show a live status chip, got: %s", body)
	}
	if !strings.Contains(body, "no deploys") {
		t.Fatalf("apps page should show a \"no deploys\" chip for the fresh app, got: %s", body)
	}
}

// TestUICreateAppFormFields checks the create-app form carries the kind and
// schedule fields the handler parses.
func TestUICreateAppFormFields(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj", ck)
	if status != http.StatusOK {
		t.Fatalf("apps page: want 200, got %d", status)
	}
	for _, want := range []string{`name="kind"`, `name="schedule"`, `name="port"`, "uiKindChange"} {
		if !strings.Contains(body, want) {
			t.Fatalf("create form missing %q, got: %s", want, body)
		}
	}
}

// TestUIEjectBanner checks the ejected-app banner text appears only for an
// ejected app.
func TestUIEjectBanner(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web2","port":8080}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eject: want 200, got %d", resp.StatusCode)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	const bannerText = "no longer manages its manifests"

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("ejected app page: want 200, got %d", status)
	}
	if !strings.Contains(body, bannerText) {
		t.Fatalf("ejected app page should show the eject banner, got: %s", body)
	}

	status, body = getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web2", ck)
	if status != http.StatusOK {
		t.Fatalf("non-ejected app page: want 200, got %d", status)
	}
	if strings.Contains(body, bannerText) {
		t.Fatalf("non-ejected app page should not show the eject banner, got: %s", body)
	}
}
