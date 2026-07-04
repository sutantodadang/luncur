package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/mail"
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

	// csrf cookie, obtained the way a browser would: load the login page.
	csrfCk := uiCSRF(t, client, srv.URL)

	// 2. POST /ui/login with form email/password + csrf -> 303 Location /ui/, Set-Cookie
	form := url.Values{"email": {"u@example.com"}, "password": {"password123"}}
	resp2 := uiPost(t, client, srv.URL+"/ui/login", csrfCk, nil, form)
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

	csrfCk := uiCSRF(t, client, srv.URL)
	form := url.Values{"email": {"u2@example.com"}, "password": {"wrongpass"}}
	resp := uiPost(t, client, srv.URL+"/ui/login", csrfCk, nil, form)
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

// uiCSRF fetches GET /ui/login the way a browser would before submitting any
// /ui/ form, and returns the luncur_csrf cookie it sets. The cookie isn't
// session-bound, so the same value works before and after login.
func uiCSRF(t *testing.T, client *http.Client, base string) *http.Cookie {
	t.Helper()
	resp, err := client.Get(base + "/ui/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == csrfCookie {
			return c
		}
	}
	t.Fatalf("GET /ui/login: expected Set-Cookie %s", csrfCookie)
	return nil
}

// uiPost POSTs a url-encoded form carrying the csrf cookie plus a matching
// _csrf field, and the session cookie if given, mirroring a real browser
// submission of a page loaded with uiCSRF's cookie.
func uiPost(t *testing.T, client *http.Client, target string, csrfCk, sessionCk *http.Cookie, form url.Values) *http.Response {
	t.Helper()
	form.Set("_csrf", csrfCk.Value)
	req, err := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCk)
	if sessionCk != nil {
		req.AddCookie(sessionCk)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
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
	if !strings.Contains(string(body), "deploys") {
		t.Fatalf("GET /ui/projects/web/apps/api: body missing metrics stats line, got: %s", body)
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
	csrfCk := uiCSRF(t, client, srv.URL)
	form := url.Values{"replicas": {"5"}}
	resp := uiPost(t, client, srv.URL+"/ui/projects/web/apps/api/scale", csrfCk, ck, form)
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
	csrfCk := uiCSRF(t, client, srv.URL)
	resp := uiPost(t, client, srv.URL+"/ui/projects/web/apps/g/deploy", csrfCk, ck, url.Values{})
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
	csrfCk := uiCSRF(t, client, srv.URL)

	addForm := url.Values{"hostname": {"www.example.com"}}
	addResp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/domains", csrfCk, ck, addForm)
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
	delResp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/domains/delete", csrfCk, ck, delForm)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST domains/delete: want 303, got %d", delResp.StatusCode)
	}

	if body := appPage(t); strings.Contains(body, "<td>www.example.com</td>") {
		t.Fatalf("app page after delete: want www.example.com removed, got: %s", body)
	}
}

// TestUIPostRequiresCSRF exercises the double-submit CSRF check on both a
// uiPage-wrapped POST (scale) and the standalone login POST.
func TestUIPostRequiresCSRF(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	sessionCk := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	scaleURL := srv.URL + "/ui/projects/web/apps/api/scale"
	scalePost := func(csrfField string) *http.Response {
		t.Helper()
		form := url.Values{"replicas": {"3"}}
		if csrfField != "" {
			form.Set("_csrf", csrfField)
		}
		req, err := http.NewRequest("POST", scaleURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(sessionCk)
		req.AddCookie(csrfCk)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// 1. No _csrf field -> 403.
	if resp := scalePost(""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scale without _csrf: want 403, got %d", resp.StatusCode)
	}

	// 2. Wrong _csrf -> 403.
	if resp := scalePost("wrong"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scale with wrong _csrf: want 403, got %d", resp.StatusCode)
	}

	// 3. Correct _csrf -> not 403.
	if resp := scalePost(csrfCk.Value); resp.StatusCode == http.StatusForbidden {
		t.Fatalf("scale with correct _csrf: want not 403, got %d", resp.StatusCode)
	}

	// 4. POST /ui/login without _csrf, after GETting /ui/login for the
	// cookie -> 403.
	loginForm := url.Values{"email": {"root@b.co"}, "password": {"pw123456"}}
	loginReq, err := http.NewRequest("POST", srv.URL+"/ui/login", strings.NewReader(loginForm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.AddCookie(csrfCk)
	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusForbidden {
		t.Fatalf("login without _csrf: want 403, got %d", loginResp.StatusCode)
	}
}

// TestUIRollback exercises the rollback button on the app detail page: a
// CSRF-correct POST with an explicit deploy_id redirects back to the app
// page, and that page's history table then shows the new row's rollback
// marker. Reuses rollback_test.go's fake-registry + kube-backed fixture,
// adapted for the UI's session-cookie login flow.
func TestUIRollback(t *testing.T) {
	registryHost := fakeRegistry(t, "proj/web:1", "proj/web:2")
	srv, st := rollbackServer(t, registryHost)

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	d1, err := st.CreateDeployment(id, "live", registryHost+"/proj/web:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(id, "live", registryHost+"/proj/web:2", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	form := url.Values{"deploy_id": {fmt.Sprintf("%d", d1.ID)}}
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/rollback", csrfCk, ck, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST rollback: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/projects/proj/apps/web" {
		t.Fatalf("POST rollback: want Location /ui/projects/proj/apps/web, got %q", loc)
	}

	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj/apps/web", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	appResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer appResp.Body.Close()
	if appResp.StatusCode != http.StatusOK {
		t.Fatalf("GET app page after rollback: want 200, got %d", appResp.StatusCode)
	}
	body, err := io.ReadAll(appResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), fmt.Sprintf("(rollback of %d)", d1.ID)) {
		t.Fatalf("app page after rollback: want rollback marker, got: %s", body)
	}
}

// TestUIRegisterFlow exercises the session-less invite-based registration
// flow: a valid token renders the form, a bogus one shows the error page, a
// successful POST logs the new user straight in and burns the invite, and
// the created user carries the invite's role.
func TestUIRegisterFlow(t *testing.T) {
	srv, st := testServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := st.CreateInvite("member", admin.ID)
	if err != nil {
		t.Fatal(err)
	}

	client := noRedirectClient()

	// 1. GET /ui/register?token=<tok> -> 200, body contains the email field.
	resp1, err := client.Get(srv.URL + "/ui/register?token=" + inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/register valid token: want 200, got %d", resp1.StatusCode)
	}
	body1, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body1), `name="email"`) {
		t.Fatalf("GET /ui/register valid token: body missing email field, got: %s", body1)
	}

	// 2. GET /ui/register?token=bogus -> 200, body contains "invalid or
	// expired", no email field.
	resp2, err := client.Get(srv.URL + "/ui/register?token=bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/register bogus token: want 200, got %d", resp2.StatusCode)
	}
	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body2), "invalid or expired") {
		t.Fatalf("GET /ui/register bogus token: body missing error, got: %s", body2)
	}
	if strings.Contains(string(body2), `name="email"`) {
		t.Fatalf("GET /ui/register bogus token: unexpected email field, got: %s", body2)
	}

	// 3. POST /ui/register (csrf-correct) -> 303 to /ui/, Set-Cookie
	// luncur_session.
	csrfCk := uiCSRF(t, client, srv.URL)
	form := url.Values{
		"email":    {"new@x.com"},
		"password": {"secret123"},
		"token":    {inv.Token},
	}
	resp3 := uiPost(t, client, srv.URL+"/ui/register", csrfCk, nil, form)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /ui/register: want 303, got %d", resp3.StatusCode)
	}
	if loc := resp3.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("POST /ui/register: want Location /ui/, got %q", loc)
	}
	var sessionCk *http.Cookie
	for _, c := range resp3.Cookies() {
		if c.Name == sessionCookie {
			sessionCk = c
		}
	}
	if sessionCk == nil {
		t.Fatal("POST /ui/register: expected Set-Cookie luncur_session")
	}

	// 4. The invite is burned: GET /ui/register?token=<tok> -> invalid page.
	resp4, err := client.Get(srv.URL + "/ui/register?token=" + inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	body4, err := io.ReadAll(resp4.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body4), "invalid or expired") {
		t.Fatalf("GET /ui/register burned token: want invalid page, got: %s", body4)
	}

	// 5. New user exists with role member.
	newUser, err := st.GetUserByEmail("new@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if newUser.Role != "member" {
		t.Fatalf("new user role: want member, got %q", newUser.Role)
	}
}

// TestUIUsersPageAdminOnly exercises the admin-only users page: a member
// gets a plain 404 (leak-nothing, mirroring uiProject), an admin sees both
// users, can create an invite (which then shows up as a registration link),
// and can delete another user but not themselves.
func TestUIUsersPageAdminOnly(t *testing.T) {
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

	// Member GETs /ui/users -> 404.
	memberCk := uiSessionCookie(t, st, member.ID)
	req, err := http.NewRequest("GET", srv.URL+"/ui/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(memberCk)
	memberResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer memberResp.Body.Close()
	if memberResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /ui/users as member: want 404, got %d", memberResp.StatusCode)
	}

	// Admin GETs -> 200 with both users' emails.
	adminCk := uiSessionCookie(t, st, admin.ID)
	usersPage := func(t *testing.T) string {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/ui/users", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(adminCk)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ui/users as admin: want 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	adminBody := usersPage(t)
	if !strings.Contains(adminBody, "root@b.co") || !strings.Contains(adminBody, "m@b.co") {
		t.Fatalf("GET /ui/users as admin: want both emails listed, got: %s", adminBody)
	}

	// Admin posts /ui/users/invite (role member) -> 303; page now shows
	// /ui/register?token=.
	csrfCk := uiCSRF(t, client, srv.URL)
	inviteForm := url.Values{"role": {"member"}}
	inviteResp := uiPost(t, client, srv.URL+"/ui/users/invite", csrfCk, adminCk, inviteForm)
	inviteResp.Body.Close()
	if inviteResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /ui/users/invite: want 303, got %d", inviteResp.StatusCode)
	}
	if body := usersPage(t); !strings.Contains(body, "/ui/register?token=") {
		t.Fatalf("GET /ui/users after invite: want registration link, got: %s", body)
	}

	// Admin posts /ui/users/delete with the member's id -> 303; page no
	// longer lists the member.
	delForm := url.Values{"id": {fmt.Sprintf("%d", member.ID)}}
	delResp := uiPost(t, client, srv.URL+"/ui/users/delete", csrfCk, adminCk, delForm)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /ui/users/delete: want 303, got %d", delResp.StatusCode)
	}
	if body := usersPage(t); strings.Contains(body, "m@b.co") {
		t.Fatalf("GET /ui/users after delete: want member removed, got: %s", body)
	}

	// Admin posting its own id -> 400.
	selfDelForm := url.Values{"id": {fmt.Sprintf("%d", admin.ID)}}
	selfDelResp := uiPost(t, client, srv.URL+"/ui/users/delete", csrfCk, adminCk, selfDelForm)
	defer selfDelResp.Body.Close()
	if selfDelResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /ui/users/delete self: want 400, got %d", selfDelResp.StatusCode)
	}
}

// extractTextarea pulls the value out of edit.html's single <textarea> via a
// plain substring split (no HTML parser needed for a fixture that only ever
// emits one), unescaping the entities html/template's text-context escaping
// introduces.
func extractTextarea(t *testing.T, body string) string {
	t.Helper()
	open := strings.Index(body, "<textarea")
	if open == -1 {
		t.Fatalf("no <textarea> in body: %s", body)
	}
	start := strings.Index(body[open:], ">")
	if start == -1 {
		t.Fatalf("unterminated <textarea> tag in body: %s", body)
	}
	start += open + 1
	end := strings.Index(body[start:], "</textarea>")
	if end == -1 {
		t.Fatalf("no closing </textarea> in body: %s", body)
	}
	return html.UnescapeString(body[start : start+end])
}

// TestUIYAMLEditor exercises the per-kind rendered-YAML editor: GET shows
// the current doc, a CSRF-correct POST with an edited replica count stores
// the diff as an override that survives redeploys, invalid YAML re-renders
// the editor with the error and the user's text preserved, and an
// unsupported kind 404s.
func TestUIYAMLEditor(t *testing.T) {
	srv, st := testServer(t) // no kube
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/p/apps", admin, `{"name":"web","port":3000}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	editURL := srv.URL + "/ui/projects/p/apps/web/edit/Deployment"

	// 1. GET edit/Deployment -> 200, textarea contains "kind: Deployment"
	// and the current replica count.
	req1, err := http.NewRequest("GET", editURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req1.AddCookie(ck)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("GET edit/Deployment: want 200, got %d", resp1.StatusCode)
	}
	body1, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatal(err)
	}
	textarea1 := extractTextarea(t, string(body1))
	if !strings.Contains(textarea1, "kind: Deployment") || !strings.Contains(textarea1, "replicas: 1") {
		t.Fatalf("GET edit/Deployment: textarea missing expected content, got: %s", textarea1)
	}

	// 2. CSRF-correct POST with replicas edited 1 -> 4 redirects to the app
	// page and stores the override.
	csrfCk := uiCSRF(t, client, srv.URL)
	edited := strings.Replace(textarea1, "replicas: 1", "replicas: 4", 1)
	postForm := url.Values{"yaml": {edited}}
	resp2 := uiPost(t, client, editURL, csrfCk, ck, postForm)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST edit/Deployment: want 303, got %d", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); loc != "/ui/projects/p/apps/web" {
		t.Fatalf("POST edit/Deployment: want Location /ui/projects/p/apps/web, got %q", loc)
	}
	id := appID(t, st, "p", "web")
	overrides, err := st.Overrides(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(overrides["Deployment"], `"replicas":4`) {
		t.Fatalf("stored override: want replicas:4, got: %s", overrides["Deployment"])
	}

	// 3. POST invalid YAML re-renders the editor (200) with the error and
	// the submitted text preserved.
	badForm := url.Values{"yaml": {"not: [valid"}}
	resp3 := uiPost(t, client, editURL, csrfCk, ck, badForm)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("POST invalid yaml: want 200, got %d", resp3.StatusCode)
	}
	body3, err := io.ReadAll(resp3.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body3), `class="err"`) {
		t.Fatalf("POST invalid yaml: want error message, got: %s", body3)
	}
	if !strings.Contains(extractTextarea(t, string(body3)), "not: [valid") {
		t.Fatalf("POST invalid yaml: want submitted text preserved, got: %s", body3)
	}

	// 4. GET edit/ConfigMap -> 404 (unsupported kind).
	req4, err := http.NewRequest("GET", srv.URL+"/ui/projects/p/apps/web/edit/ConfigMap", nil)
	if err != nil {
		t.Fatal(err)
	}
	req4.AddCookie(ck)
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("GET edit/ConfigMap: want 404, got %d", resp4.StatusCode)
	}
}

// TestUIAddons exercises the project-page create/delete and app-page
// attach/detach addon forms end to end, reusing addons_test.go's
// fake-kube fixture (addonTestServer) since these handlers need a kube
// client to provision/delete cluster objects.
func TestUIAddons(t *testing.T) {
	_, srv, st, _ := addonTestServer(t)
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

	projectPage := func(t *testing.T) string {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj", nil)
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
			t.Fatalf("GET project page: want 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
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

	// 1. Create via the project page's create form -> 303, addon listed.
	createResp := uiPost(t, client, srv.URL+"/ui/projects/proj/addons", csrfCk, ck, url.Values{"type": {"postgres"}})
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST addons create: want 303, got %d", createResp.StatusCode)
	}
	if body := projectPage(t); !strings.Contains(body, "<td>postgres1</td>") {
		t.Fatalf("project page after create: want postgres1 listed, got: %s", body)
	}

	// 2. Attach via the app page's attach form -> 303, addon shown attached.
	attachResp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/addons/attach", csrfCk, ck, url.Values{"name": {"postgres1"}})
	attachResp.Body.Close()
	if attachResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST addons attach: want 303, got %d", attachResp.StatusCode)
	}
	if body := appPage(t); !strings.Contains(body, "<td>postgres1</td>") {
		t.Fatalf("app page after attach: want postgres1 listed, got: %s", body)
	}

	// 3. Detach -> 303, no longer in the attached list.
	detachResp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/addons/detach", csrfCk, ck, url.Values{"name": {"postgres1"}})
	detachResp.Body.Close()
	if detachResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST addons detach: want 303, got %d", detachResp.StatusCode)
	}
	if body := appPage(t); strings.Contains(body, "<td>postgres1</td>") {
		t.Fatalf("app page after detach: want postgres1 removed, got: %s", body)
	}

	// 4. Delete with force from the project page -> 303, gone from the list.
	deleteResp := uiPost(t, client, srv.URL+"/ui/projects/proj/addons/delete", csrfCk, ck,
		url.Values{"name": {"postgres1"}, "force": {"1"}})
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST addons delete: want 303, got %d", deleteResp.StatusCode)
	}
	if body := projectPage(t); strings.Contains(body, "<td>postgres1</td>") {
		t.Fatalf("project page after delete: want postgres1 removed, got: %s", body)
	}
}

// TestUIAppPageShowsEjected ejects an app directly through the store (the
// HTTP eject flow is covered by eject_test.go) and checks the app page's
// template guards: an "ejected" badge appears, and the scale form — a
// stand-in for every mutation form the template hides — is gone.
func TestUIAppPageShowsEjected(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/p/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	if err := st.SetAppEjected(appID(t, st, "p", "web")); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/p/apps/web", nil)
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
	if !strings.Contains(string(body), "ejected") {
		t.Fatalf("app page: want ejected badge, got: %s", body)
	}
	if strings.Contains(string(body), `action="/ui/projects/p/apps/web/scale"`) {
		t.Fatalf("app page: want scale form hidden for an ejected app, got: %s", body)
	}
}

// TestUIInviteEmailNote: the invite form with an email posts, sends via
// the mailer, and redirects to a page that shows the outcome note.
func TestUIInviteEmailNote(t *testing.T) {
	st := newTestStore(t)
	s := newServer(Deps{Store: st})
	fm := &fakeMailer{}
	s.mailer = func() (mail.Mailer, error) { return fm, nil }
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/users/invite", csrfCk, adminCk,
		url.Values{"role": {"member"}, "email": {"new@b.co"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invite post: want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/ui/users?mail=sent" {
		t.Fatalf("redirect = %q, want /ui/users?mail=sent", loc)
	}
	if fm.to != "new@b.co" {
		t.Fatalf("mail to = %q, want new@b.co", fm.to)
	}

	// The redirected-to page renders the note.
	req, err := http.NewRequest("GET", srv.URL+loc, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(adminCk)
	page, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "invite emailed") {
		t.Fatalf("users page missing mail note:\n%s", body)
	}

	// Failure path: mailer errors -> ?mail=failed, invite still created.
	fm.err = fmt.Errorf("boom")
	resp = uiPost(t, client, srv.URL+"/ui/users/invite", csrfCk, adminCk,
		url.Values{"role": {"member"}, "email": {"x@b.co"}})
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/ui/users?mail=failed" {
		t.Fatalf("redirect = %q, want /ui/users?mail=failed", got)
	}
	invs, err := st.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 2 {
		t.Fatalf("invites = %d, want 2", len(invs))
	}
}

// TestUIAdoptButton: an ejected app's page shows the adopt form; posting it
// clears the flag and the page returns to normal management UI.
func TestUIAdoptButton(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "").Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	ck := uiSessionCookie(t, st, u.ID)

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
			t.Fatalf("app page: want 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	body := appPage(t)
	if !strings.Contains(body, `action="/ui/projects/proj/apps/web/adopt"`) {
		t.Fatalf("ejected page missing adopt form:\n%s", body)
	}

	csrfCk := uiCSRF(t, client, srv.URL)
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/adopt", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adopt post: want 303, got %d", resp.StatusCode)
	}

	body = appPage(t)
	if strings.Contains(body, "This app is ejected") {
		t.Fatalf("page still shows ejected note after adopt:\n%s", body)
	}
	if !strings.Contains(body, `action="/ui/projects/proj/apps/web/scale"`) {
		t.Fatalf("management UI (scale form) not back after adopt:\n%s", body)
	}

	// Adopt on a non-ejected app -> 409.
	resp = uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/adopt", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("adopt non-ejected: want 409, got %d", resp.StatusCode)
	}
}

// TestUITokensPage: any user sees their own tokens (the web session shows
// as "session"), revoking an API token removes it, and revoking the
// session token logs the browser out.
func TestUITokensPage(t *testing.T) {
	srv, st := testServer(t)
	apiTok := seedUserToken(t, st, "u@b.co", "member")
	_ = apiTok

	u, err := st.GetUserByEmail("u@b.co")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	ck := uiSessionCookie(t, st, u.ID)

	tokensPage := func(t *testing.T) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/ui/tokens", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(ck)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	code, body := tokensPage(t)
	if code != http.StatusOK {
		t.Fatalf("tokens page: want 200, got %d", code)
	}
	if !strings.Contains(body, "session") {
		t.Fatalf("tokens page missing session token:\n%s", body)
	}
	if !strings.Contains(body, `action="/ui/tokens/revoke"`) {
		t.Fatalf("tokens page missing revoke form:\n%s", body)
	}
	// Nav link present.
	if !strings.Contains(body, `href="/ui/tokens"`) {
		t.Fatalf("nav missing tokens link:\n%s", body)
	}

	// Two tokens: seedUserToken's API token (older), then the session
	// minted by uiSessionCookie (newer). ListTokens is newest-first. (The
	// helper names its session "test"; the real login flow names it
	// "session" — the ordering, not the name, identifies it here.)
	tokens, err := st.ListTokens(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("tokens = %+v", tokens)
	}
	sessionID, apiID := tokens[0].ID, tokens[1].ID

	csrfCk := uiCSRF(t, client, srv.URL)
	resp := uiPost(t, client, srv.URL+"/ui/tokens/revoke", csrfCk, ck,
		url.Values{"id": {strconv.FormatInt(apiID, 10)}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: want 303, got %d", resp.StatusCode)
	}
	if left, _ := st.ListTokens(u.ID); len(left) != 1 {
		t.Fatalf("tokens after revoke = %+v", left)
	}

	// Revoking the session token logs the browser out: the next page load
	// redirects to /ui/login.
	resp = uiPost(t, client, srv.URL+"/ui/tokens/revoke", csrfCk, ck,
		url.Values{"id": {strconv.FormatInt(sessionID, 10)}})
	resp.Body.Close()
	code, _ = tokensPage(t)
	if code != http.StatusSeeOther {
		t.Fatalf("after session revoke: want 303 to login, got %d", code)
	}
}
