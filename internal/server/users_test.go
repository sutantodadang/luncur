package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func doAuthed(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func seedUserToken(t *testing.T, st *store.Store, email, role string) string {
	t.Helper()
	u, err := st.CreateUser(email, "pw123456", role)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMeRequiresAuth(t *testing.T) {
	srv, st := testServer(t)
	resp := doAuthed(t, "GET", srv.URL+"/v1/me", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token: want 401, got %d", resp.StatusCode)
	}

	tok := seedUserToken(t, st, "me@b.co", "member")
	ok := doAuthed(t, "GET", srv.URL+"/v1/me", tok, "")
	defer ok.Body.Close()
	if ok.StatusCode != 200 {
		t.Fatalf("with token: want 200, got %d", ok.StatusCode)
	}
	var me struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.Email != "me@b.co" || me.Role != "member" {
		t.Fatalf("bad me: %+v", me)
	}
}

func TestCreateUserAdminOnly(t *testing.T) {
	srv, st := testServer(t)
	adminTok := seedUserToken(t, st, "root@b.co", "admin")
	memberTok := seedUserToken(t, st, "pleb@b.co", "member")

	body := `{"email":"new@b.co","password":"pw123456","role":"member"}`

	forbidden := doAuthed(t, "POST", srv.URL+"/v1/users", memberTok, body)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("member: want 403, got %d", forbidden.StatusCode)
	}

	created := doAuthed(t, "POST", srv.URL+"/v1/users", adminTok, body)
	defer created.Body.Close()
	if created.StatusCode != 201 {
		t.Fatalf("admin: want 201, got %d", created.StatusCode)
	}
	if _, err := st.Authenticate("new@b.co", "pw123456"); err != nil {
		t.Fatalf("new user cannot authenticate: %v", err)
	}
}
