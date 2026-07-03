package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func TestUILoginFlow(t *testing.T) {
	srv, st := testServer(t)
	if _, err := st.CreateUser("u@example.com", "password123", "member"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. GET /ui/ without cookie -> 303 Location /ui/login
	resp1, err := client.Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /ui/ no cookie: want 303, got %d", resp1.StatusCode)
	}
	if loc := resp1.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("GET /ui/ no cookie: want Location /ui/login, got %q", loc)
	}

	// 2. POST /ui/login with form email/password -> 303 Location /ui/, Set-Cookie
	form := url.Values{"email": {"u@example.com"}, "password": {"password123"}}
	resp2, err := client.Post(srv.URL+"/ui/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /ui/login: want 303, got %d", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("POST /ui/login: want Location /ui/, got %q", loc)
	}
	var sessionCk *http.Cookie
	for _, c := range resp2.Cookies() {
		if c.Name == "luncur_session" {
			sessionCk = c
		}
	}
	if sessionCk == nil {
		t.Fatal("POST /ui/login: expected Set-Cookie luncur_session")
	}
	if !sessionCk.HttpOnly {
		t.Fatal("session cookie: want HttpOnly")
	}
	if sessionCk.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: want SameSite=Strict, got %v", sessionCk.SameSite)
	}

	// 3. GET /ui/ with cookie -> 200, body contains "Projects"
	req3, err := http.NewRequest("GET", srv.URL+"/ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req3.AddCookie(sessionCk)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ with cookie: want 200, got %d", resp3.StatusCode)
	}
	body3, err := io.ReadAll(resp3.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body3), "Projects") {
		t.Fatalf("GET /ui/ with cookie: body missing Projects, got: %s", body3)
	}

	// 4. API also accepts the cookie: GET /v1/me with only the cookie -> 200
	req4, err := http.NewRequest("GET", srv.URL+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req4.AddCookie(sessionCk)
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me with cookie only: want 200, got %d", resp4.StatusCode)
	}
}

func TestUILoginBadPassword(t *testing.T) {
	srv, st := testServer(t)
	if _, err := st.CreateUser("u2@example.com", "password123", "member"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	form := url.Values{"email": {"u2@example.com"}, "password": {"wrongpass"}}
	resp, err := client.Post(srv.URL+"/ui/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bad password: want 200, got %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "luncur_session" {
			t.Fatal("bad password: unexpected Set-Cookie luncur_session")
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "wrong email or password") {
		t.Fatalf("bad password: body missing error message, got: %s", body)
	}
}

func TestRootRedirectsToUI(t *testing.T) {
	srv, _ := testServer(t)
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("GET /: want Location /ui/, got %q", loc)
	}
}

func TestBearerWinsOverCookie(t *testing.T) {
	srv, st := testServer(t)
	bearerTok := seedUserToken(t, st, "bearer@example.com", "member")
	cookieTok := seedUserToken(t, st, "cookie@example.com", "member")

	req, err := http.NewRequest("GET", srv.URL+"/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerTok)
	req.AddCookie(&http.Cookie{Name: "luncur_session", Value: cookieTok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var me struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.Email != "bearer@example.com" {
		t.Fatalf("bearer must win over cookie: got %q", me.Email)
	}
}

// uiSessionCookie mints a session token for a user directly against the
// store (bypassing the /ui/login HTTP round trip) and wraps it as the
// cookie uiPage expects.
func uiSessionCookie(t *testing.T, st interface {
	CreateSessionToken(int64, string) (string, error)
}, userID int64) *http.Cookie {
	t.Helper()
	tok, err := st.CreateSessionToken(userID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestUIProjectVisibleOnList(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/", nil)
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
		t.Fatalf("GET /ui/: want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `/ui/projects/web`) {
		t.Fatalf("GET /ui/: body missing project link, got: %s", body)
	}
}

func TestUIAppVisibleOnProjectPage(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/web", nil)
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
		t.Fatalf("GET /ui/projects/web: want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `/ui/projects/web/apps/api`) {
		t.Fatalf("GET /ui/projects/web: body missing app link, got: %s", body)
	}
}

func TestUIAppDetailShowsStatus(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	id := appID(t, st, "web", "api")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/web/apps/api", nil)
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
		t.Fatalf("GET /ui/projects/web/apps/api: want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// class="status-live" (the rendered attribute) is distinct from the
	// base stylesheet's ".status-live{...}" CSS rule, which is present on
	// every page regardless of this app's actual status.
	if !strings.Contains(string(body), `class="status-live"`) {
		t.Fatalf("GET /ui/projects/web/apps/api: body missing live status, got: %s", body)
	}
}

// TestUIScalePersists mirrors TestScaleLiveAppWithoutKube503LeavesReplicasUnchanged's
// setup but with a non-live app, so the nil-kube deps in testServer never
// need a live kube client: POST scale should succeed, persist, and redirect
// with 303.
func TestUIScalePersists(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	form := url.Values{"replicas": {"5"}}
	req, err := http.NewRequest("POST", srv.URL+"/ui/projects/web/apps/api/scale", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST scale: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/web/apps/api" {
		t.Fatalf("POST scale: want Location /ui/projects/web/apps/api, got %q", loc)
	}

	a, err := st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil {
		t.Fatal(err)
	}
	if a.Replicas != 5 {
		t.Fatalf("replicas: want 5, got %d", a.Replicas)
	}
}

// TestUIDeployGitAppWithoutKube503 guards the UI git-deploy path when the
// server has no kube client: it must answer 503 (mirroring the API's
// kubernetes_unavailable), NOT silently redirect, and must not create a
// deployment row it could never build.
func TestUIDeployGitAppWithoutKube503(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("POST", srv.URL+"/ui/projects/web/apps/g/deploy", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST deploy without kube: want 503, got %d", resp.StatusCode)
	}

	if _, err := st.LatestDeployment(appID(t, st, "web", "g")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no deployment row must be created, got err=%v", err)
	}
}

func TestUIMemberCannotSeeForeignProject(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, member.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/web", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /ui/projects/web as non-member: want 404, got %d", resp.StatusCode)
	}
}

func TestUIAppDetailContainsEventSourceScript(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	id := appID(t, st, "web", "api")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/web/apps/api", nil)
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
		t.Fatalf("GET /ui/projects/web/apps/api: want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "new EventSource") {
		t.Fatalf("app detail page: body missing 'new EventSource', got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `id="logs"`) {
		t.Fatalf("app detail page: body missing 'id=\"logs\"', got: %s", bodyStr)
	}
}

// TestUIDomainAddAndDelete exercises the Domains section end to end: a
// logged-in admin adds a domain via the UI form (303 back to the app page),
// the app page then lists it, and removing it via the delete form makes it
// disappear again.
func TestUIDomainAddAndDelete(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":3000}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	addForm := url.Values{"hostname": {"www.example.com"}}
	addReq, err := http.NewRequest("POST", srv.URL+"/ui/projects/proj/apps/web/domains", strings.NewReader(addForm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	addReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addReq.AddCookie(ck)
	addResp, err := client.Do(addReq)
	if err != nil {
		t.Fatal(err)
	}
	addResp.Body.Close()
	if addResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST domains: want 303, got %d", addResp.StatusCode)
	}

	appPage := func(t *testing.T) string {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj/apps/web", nil)
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
			t.Fatalf("GET app page: want 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	// Look for the hostname inside a table cell rather than as a bare
	// substring: the add-domain form's placeholder text is also
	// "www.example.com" and is present on the page regardless of whether
	// any domain row exists.
	if body := appPage(t); !strings.Contains(body, "<td>www.example.com</td>") {
		t.Fatalf("app page after add: want www.example.com listed, got: %s", body)
	}

	delForm := url.Values{"hostname": {"www.example.com"}}
	delReq, err := http.NewRequest("POST", srv.URL+"/ui/projects/proj/apps/web/domains/delete", strings.NewReader(delForm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delReq.AddCookie(ck)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST domains/delete: want 303, got %d", delResp.StatusCode)
	}

	if body := appPage(t); strings.Contains(body, "<td>www.example.com</td>") {
		t.Fatalf("app page after delete: want www.example.com removed, got: %s", body)
	}
}
