package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestChangePasswordAPI(t *testing.T) {
	srv, st := testServer(t)
	tok := seedUserToken(t, st, "me@b.co", "member")

	wrong := doAuthed(t, "PUT", srv.URL+"/v1/me/password", tok, `{"old_password":"nope","new_password":"newpassword1"}`)
	defer wrong.Body.Close()
	if wrong.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong old password: want 403, got %d", wrong.StatusCode)
	}

	short := doAuthed(t, "PUT", srv.URL+"/v1/me/password", tok, `{"old_password":"pw123456","new_password":"short"}`)
	defer short.Body.Close()
	if short.StatusCode != http.StatusBadRequest {
		t.Fatalf("short new password: want 400, got %d", short.StatusCode)
	}

	ok := doAuthed(t, "PUT", srv.URL+"/v1/me/password", tok, `{"old_password":"pw123456","new_password":"newpassword1"}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusNoContent {
		t.Fatalf("change password: want 204, got %d", ok.StatusCode)
	}

	loginNew := postJSON(t, srv.URL+"/v1/login", `{"email":"me@b.co","password":"newpassword1"}`)
	defer loginNew.Body.Close()
	if loginNew.StatusCode != http.StatusOK {
		t.Fatalf("login with new password: want 200, got %d", loginNew.StatusCode)
	}

	loginOld := postJSON(t, srv.URL+"/v1/login", `{"email":"me@b.co","password":"pw123456"}`)
	defer loginOld.Body.Close()
	if loginOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login with old password: want 401, got %d", loginOld.StatusCode)
	}
}

func TestChangeEmailAPI(t *testing.T) {
	srv, st := testServer(t)
	tok := seedUserToken(t, st, "me2@b.co", "member")
	seedUserToken(t, st, "taken@b.co", "member")

	wrong := doAuthed(t, "PUT", srv.URL+"/v1/me/email", tok, `{"password":"nope","email":"new@b.co"}`)
	defer wrong.Body.Close()
	if wrong.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong password: want 403, got %d", wrong.StatusCode)
	}

	taken := doAuthed(t, "PUT", srv.URL+"/v1/me/email", tok, `{"password":"pw123456","email":"taken@b.co"}`)
	defer taken.Body.Close()
	if taken.StatusCode != http.StatusConflict {
		t.Fatalf("taken email: want 409, got %d", taken.StatusCode)
	}

	ok := doAuthed(t, "PUT", srv.URL+"/v1/me/email", tok, `{"password":"pw123456","email":" New@B.co "}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("change email: want 200, got %d", ok.StatusCode)
	}
	var out struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Email != "new@b.co" {
		t.Fatalf("want normalized email new@b.co, got %q", out.Email)
	}

	login := postJSON(t, srv.URL+"/v1/login", `{"email":"new@b.co","password":"pw123456"}`)
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login with new email: want 200, got %d", login.StatusCode)
	}
}

func TestAdminSetPasswordAPI(t *testing.T) {
	srv, st := testServer(t)
	adminTok := seedUserToken(t, st, "admin3@b.co", "admin")
	memberTok := seedUserToken(t, st, "member3@b.co", "member")
	member, err := st.GetUserByEmail("member3@b.co")
	if err != nil {
		t.Fatal(err)
	}

	forbidden := doAuthed(t, "PUT", memberURL(srv.URL, member.ID), memberTok, `{"password":"newpassword1"}`)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("member reset: want 403, got %d", forbidden.StatusCode)
	}

	missing := doAuthed(t, "PUT", memberURL(srv.URL, 999999), adminTok, `{"password":"newpassword1"}`)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %d", missing.StatusCode)
	}

	short := doAuthed(t, "PUT", memberURL(srv.URL, member.ID), adminTok, `{"password":"short"}`)
	defer short.Body.Close()
	if short.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password: want 400, got %d", short.StatusCode)
	}

	ok := doAuthed(t, "PUT", memberURL(srv.URL, member.ID), adminTok, `{"password":"newpassword1"}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusNoContent {
		t.Fatalf("admin reset: want 204, got %d", ok.StatusCode)
	}
	if _, err := st.Authenticate("member3@b.co", "newpassword1"); err != nil {
		t.Fatalf("member cannot login with reset password: %v", err)
	}
}

func TestUIAccountPage(t *testing.T) {
	srv, st := testServer(t)
	u, err := st.CreateUser("uiacct@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)

	client := noRedirectClient()
	req, err := http.NewRequest("GET", srv.URL+"/ui/account", nil)
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
		t.Fatalf("GET /ui/account: want 200, got %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "Change password") {
		t.Fatalf("body missing Change password: %s", body)
	}
	if !strings.Contains(body, "luncur account passwd") {
		t.Fatalf("body missing cli-echo: %s", body)
	}
}

func TestUIAccountPasswordFlow(t *testing.T) {
	srv, st := testServer(t)
	u, err := st.CreateUser("uiflow@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	sessionCk := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	wrong := uiPost(t, client, srv.URL+"/ui/account/password", csrfCk, sessionCk,
		url.Values{"old": {"nope"}, "new": {"newpassword1"}})
	defer wrong.Body.Close()
	if wrong.StatusCode != http.StatusSeeOther {
		t.Fatalf("wrong old: want 303, got %d", wrong.StatusCode)
	}
	if loc := wrong.Header.Get("Location"); loc != "/ui/account?err=wrong" {
		t.Fatalf("wrong old: want Location /ui/account?err=wrong, got %q", loc)
	}

	ok := uiPost(t, client, srv.URL+"/ui/account/password", csrfCk, sessionCk,
		url.Values{"old": {"password123"}, "new": {"newpassword1"}})
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct old: want 303, got %d", ok.StatusCode)
	}
	if loc := ok.Header.Get("Location"); loc != "/ui/account?ok=password" {
		t.Fatalf("correct old: want Location /ui/account?ok=password, got %q", loc)
	}
	if _, err := st.Authenticate("uiflow@b.co", "newpassword1"); err != nil {
		t.Fatalf("password not actually changed: %v", err)
	}
}

func TestUIAdminResetPassword(t *testing.T) {
	srv, st := testServer(t)
	admin, err := st.CreateUser("uiadmin@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("uimember@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	adminCk := uiSessionCookie(t, st, admin.ID)
	memberCk := uiSessionCookie(t, st, member.ID)

	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/users/password", csrfCk, adminCk,
		url.Values{"id": {strconv.FormatInt(member.ID, 10)}, "password": {"resetpassword1"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin reset: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/users?pw=ok" {
		t.Fatalf("admin reset: want Location /ui/users?pw=ok, got %q", loc)
	}
	if _, err := st.Authenticate("uimember@b.co", "resetpassword1"); err != nil {
		t.Fatalf("member cannot login with reset password: %v", err)
	}

	forbidden := uiPost(t, client, srv.URL+"/ui/users/password", csrfCk, memberCk,
		url.Values{"id": {strconv.FormatInt(admin.ID, 10)}, "password": {"anotherpassword1"}})
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusNotFound {
		t.Fatalf("non-admin reset: want 404 (uiAdmin's leak-nothing policy), got %d", forbidden.StatusCode)
	}
}

func memberURL(base string, id int64) string {
	return base + "/v1/users/" + strconv.FormatInt(id, 10) + "/password"
}
