package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/mail"
	"github.com/sutantodadang/luncur/internal/store"
)

func TestInviteEndpointsAdminOnly(t *testing.T) {
	srv, st := testServer(t)
	adminTok := seedUserToken(t, st, "inv-admin@b.co", "admin")
	memberTok := seedUserToken(t, st, "inv-member@b.co", "member")

	// Member cannot create invites.
	forbidden := doAuthed(t, "POST", srv.URL+"/v1/invites", memberTok, `{"role":"member"}`)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("member create: want 403, got %d", forbidden.StatusCode)
	}

	// Admin creates one.
	created := doAuthed(t, "POST", srv.URL+"/v1/invites", adminTok, `{"role":"member"}`)
	defer created.Body.Close()
	if created.StatusCode != 201 {
		t.Fatalf("create: want 201, got %d", created.StatusCode)
	}
	var inv struct {
		Token string `json:"token"`
		Role  string `json:"role"`
		Path  string `json:"path"`
		Used  bool   `json:"used"`
	}
	if err := json.NewDecoder(created.Body).Decode(&inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Token) != 32 || inv.Role != "member" || inv.Path != "/ui/register?token="+inv.Token {
		t.Fatalf("invite = %+v", inv)
	}

	// List shows it, unused.
	list := doAuthed(t, "GET", srv.URL+"/v1/invites", adminTok, "")
	defer list.Body.Close()
	var invs []struct {
		Token string `json:"token"`
		Used  bool   `json:"used"`
	}
	if err := json.NewDecoder(list.Body).Decode(&invs); err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || invs[0].Token != inv.Token || invs[0].Used {
		t.Fatalf("list = %+v", invs)
	}

	// Revoke, then list is empty.
	rev := doAuthed(t, "DELETE", srv.URL+"/v1/invites/"+inv.Token, adminTok, "")
	rev.Body.Close()
	if rev.StatusCode != 204 {
		t.Fatalf("revoke: want 204, got %d", rev.StatusCode)
	}
	list2 := doAuthed(t, "GET", srv.URL+"/v1/invites", adminTok, "")
	defer list2.Body.Close()
	invs = nil
	if err := json.NewDecoder(list2.Body).Decode(&invs); err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("after revoke: %+v", invs)
	}
}

func TestUsersListAndDelete(t *testing.T) {
	srv, st := testServer(t)
	adminTok := seedUserToken(t, st, "u-admin@b.co", "admin")
	memberTok := seedUserToken(t, st, "u-member@b.co", "member")

	// Member cannot list users.
	forbidden := doAuthed(t, "GET", srv.URL+"/v1/users", memberTok, "")
	forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("member list: want 403, got %d", forbidden.StatusCode)
	}

	list := doAuthed(t, "GET", srv.URL+"/v1/users", adminTok, "")
	defer list.Body.Close()
	if list.StatusCode != 200 {
		t.Fatalf("list: want 200, got %d", list.StatusCode)
	}
	var users []struct {
		ID         int64  `json:"id"`
		Email      string `json:"email"`
		TokenCount int64  `json:"token_count"`
	}
	if err := json.NewDecoder(list.Body).Decode(&users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %+v", users)
	}
	var adminID, memberID int64
	for _, u := range users {
		if u.TokenCount != 1 {
			t.Fatalf("token count: %+v", u)
		}
		switch u.Email {
		case "u-admin@b.co":
			adminID = u.ID
		case "u-member@b.co":
			memberID = u.ID
		}
	}

	// Self-delete forbidden.
	self := doAuthed(t, "DELETE", fmt.Sprintf("%s/v1/users/%d", srv.URL, adminID), adminTok, "")
	self.Body.Close()
	if self.StatusCode != 400 {
		t.Fatalf("self delete: want 400, got %d", self.StatusCode)
	}

	// Delete the member; their token stops working.
	del := doAuthed(t, "DELETE", fmt.Sprintf("%s/v1/users/%d", srv.URL, memberID), adminTok, "")
	del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("delete: want 204, got %d", del.StatusCode)
	}
	dead := doAuthed(t, "GET", srv.URL+"/v1/me", memberTok, "")
	dead.Body.Close()
	if dead.StatusCode != 401 {
		t.Fatalf("deleted user's token: want 401, got %d", dead.StatusCode)
	}

	// Unknown id → 404.
	gone := doAuthed(t, "DELETE", srv.URL+"/v1/users/99999", adminTok, "")
	gone.Body.Close()
	if gone.StatusCode != 404 {
		t.Fatalf("unknown delete: want 404, got %d", gone.StatusCode)
	}
}

// fakeMailer records the one message it was asked to send.
type fakeMailer struct {
	to, subject, body string
	err               error
}

func (f *fakeMailer) Send(to, subject, body string) error {
	f.to, f.subject, f.body = to, subject, body
	return f.err
}

// mailerServer builds a test server whose mailer factory is overridden.
func mailerServer(t *testing.T, m mail.Mailer, merr error) (*httptest.Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	s := newServer(Deps{Store: st})
	s.mailer = func() (mail.Mailer, error) { return m, merr }
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestInviteEmailSent(t *testing.T) {
	fm := &fakeMailer{}
	srv, st := mailerServer(t, fm, nil)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"role":"member","email":"new@b.co"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out["emailed"] != true {
		t.Fatalf("emailed = %v, want true", out["emailed"])
	}
	if _, ok := out["warning"]; ok {
		t.Fatalf("unexpected warning: %v", out["warning"])
	}
	if fm.to != "new@b.co" {
		t.Fatalf("mail to = %q, want new@b.co", fm.to)
	}
	if !strings.Contains(fm.body, "http://") || !strings.Contains(fm.body, "/ui/register?token="+out["token"].(string)) {
		t.Fatalf("mail body missing absolute register link:\n%s", fm.body)
	}
}

func TestInviteEmailSendFailure(t *testing.T) {
	fm := &fakeMailer{err: fmt.Errorf("connection refused")}
	srv, st := mailerServer(t, fm, nil)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"email":"new@b.co"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("send failure must not block creation: want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out["emailed"] != false {
		t.Fatalf("emailed = %v, want false", out["emailed"])
	}
	w, _ := out["warning"].(string)
	if !strings.Contains(w, "connection refused") {
		t.Fatalf("warning = %q, want send error in it", w)
	}

	// Invite still exists.
	invs, err := st.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("invites = %d, want 1", len(invs))
	}
}

func TestInviteEmailUnconfigured(t *testing.T) {
	// Real default mailer factory, no smtp_host setting -> ErrUnconfigured.
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"email":"new@b.co"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out["emailed"] != false {
		t.Fatalf("emailed = %v, want false", out["emailed"])
	}
	w, _ := out["warning"].(string)
	if !strings.Contains(w, "smtp is not configured") {
		t.Fatalf("warning = %q, want smtp is not configured", w)
	}
}

func TestInviteNoEmailFieldNoEmailedKey(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"role":"member"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, ok := out["emailed"]; ok {
		t.Fatalf("emailed key present without email request: %v", out)
	}
}
