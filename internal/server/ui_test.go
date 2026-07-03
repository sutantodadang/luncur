package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
