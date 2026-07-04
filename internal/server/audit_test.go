package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func auditCount(t *testing.T, st *store.Store) int {
	t.Helper()
	list, err := st.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// TestAuditRecordsSuccessfulMutation checks the base case: an authed POST
// mutation gets one row whose action/target come from the matched route.
func TestAuditRecordsSuccessfulMutation(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "audit-admin@b.co", "admin")

	before := auditCount(t, st)
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"auditproj"}`)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create project: want 201, got %d", resp.StatusCode)
	}
	list, err := st.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != before+1 {
		t.Fatalf("audit rows = %d, want %d", len(list), before+1)
	}
	row := list[0]
	if row.UserEmail != "audit-admin@b.co" || row.Action != "POST /v1/projects" || row.Target != "/v1/projects" {
		t.Fatalf("row = %+v", row)
	}
}

func TestAuditSkipsGET(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "audit-get@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()

	before := auditCount(t, st)
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects", admin, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("list projects: want 200, got %d", resp.StatusCode)
	}
	if got := auditCount(t, st); got != before {
		t.Fatalf("GET recorded an audit row: before=%d after=%d", before, got)
	}
}

func TestAuditSkipsUnauthenticatedMutation(t *testing.T) {
	srv, st := testServer(t)
	before := auditCount(t, st)
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects", "", `{"name":"noauth"}`)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token: want 401, got %d", resp.StatusCode)
	}
	if got := auditCount(t, st); got != before {
		t.Fatalf("unauthenticated mutation recorded an audit row: before=%d after=%d", before, got)
	}
}

func TestAuditSkipsFailedMutation(t *testing.T) {
	srv, st := testServer(t)
	member := seedUserToken(t, st, "audit-member@b.co", "member")

	before := auditCount(t, st)
	// Members cannot create projects -> 403.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects", member, `{"name":"nope"}`)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("member create project: want 403, got %d", resp.StatusCode)
	}
	if got := auditCount(t, st); got != before {
		t.Fatalf("failed mutation recorded an audit row: before=%d after=%d", before, got)
	}
}

// TestAuditRecordsUIFormPost mirrors ui_test.go's session-cookie + CSRF
// submission flow for a uiPage-wrapped POST.
func TestAuditRecordsUIFormPost(t *testing.T) {
	srv, st := testServer(t)
	seedUserToken(t, st, "audit-ui@b.co", "admin")
	u, err := st.GetUserByEmail("audit-ui@b.co")
	if err != nil {
		t.Fatal(err)
	}

	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)
	sessionCk := uiSessionCookie(t, st, u.ID)

	before := auditCount(t, st)
	resp := uiPost(t, client, srv.URL+"/ui/users/invite", csrfCk, sessionCk, url.Values{"role": {"member"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ui invite create: want 303, got %d", resp.StatusCode)
	}
	list, err := st.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != before+1 {
		t.Fatalf("audit rows = %d, want %d", len(list), before+1)
	}
	row := list[0]
	if row.UserEmail != "audit-ui@b.co" || row.Action != "POST /ui/users/invite" {
		t.Fatalf("row = %+v", row)
	}
}

// TestAuditRecordsLogin checks the two API-login cases: success gets a row
// with the fixed action name, failure gets none.
func TestAuditRecordsLogin(t *testing.T) {
	srv, st := testServer(t)
	if _, err := st.CreateUser("audit-login@b.co", "pw123456", "member"); err != nil {
		t.Fatal(err)
	}

	before := auditCount(t, st)
	resp := postJSON(t, srv.URL+"/v1/login", `{"email":"audit-login@b.co","password":"pw123456"}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login: want 200, got %d", resp.StatusCode)
	}
	list, err := st.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != before+1 {
		t.Fatalf("audit rows = %d, want %d", len(list), before+1)
	}
	if list[0].Action != "POST /v1/login" || list[0].UserEmail != "audit-login@b.co" {
		t.Fatalf("row = %+v", list[0])
	}

	before2 := auditCount(t, st)
	bad := postJSON(t, srv.URL+"/v1/login", `{"email":"audit-login@b.co","password":"wrong"}`)
	bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Fatalf("bad login: want 401, got %d", bad.StatusCode)
	}
	if got := auditCount(t, st); got != before2 {
		t.Fatalf("failed login recorded a row: before=%d after=%d", before2, got)
	}
}

// TestAuditRecordsWebhookTrigger reuses webhook_test.go's HMAC fixtures to
// check a valid webhook push records a row under the "webhook" user.
func TestAuditRecordsWebhookTrigger(t *testing.T) {
	srv, st := webhookTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()
	path, secretHex := decodeWebhookEnable(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/g/webhook", admin, ""))

	before := auditCount(t, st)
	body := []byte(`{"ref":"refs/heads/main"}`)
	resp := postWebhook(t, srv.URL+path, map[string]string{"X-Hub-Signature-256": githubSig(secretHex, body)}, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook trigger: want 202, got %d", resp.StatusCode)
	}
	list, err := st.ListAudit(0, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != before+1 {
		t.Fatalf("audit rows = %d, want %d", len(list), before+1)
	}
	if list[0].UserEmail != "webhook" || list[0].Action != "POST /hooks/apps/{project}/{app}" {
		t.Fatalf("row = %+v", list[0])
	}
}

// TestAuditRedactsInviteToken checks the one route whose path carries a
// secret: DELETE /v1/invites/{token}. The stored target must be the route
// pattern, never the raw token, anywhere in the audit_log table.
func TestAuditRedactsInviteToken(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "audit-invite@b.co", "admin")

	created := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"role":"member"}`)
	var inv struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(created.Body).Decode(&inv); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()

	resp := doAuthed(t, "DELETE", srv.URL+"/v1/invites/"+inv.Token, admin, "")
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("revoke invite: want 204, got %d", resp.StatusCode)
	}

	rows, err := st.DB().Query(`SELECT action, target FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var action, target string
		if err := rows.Scan(&action, &target); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(action, inv.Token) || strings.Contains(target, inv.Token) {
			t.Fatalf("raw invite token leaked into audit_log: action=%q target=%q", action, target)
		}
		if action == "DELETE /v1/invites/{token}" {
			found = true
			if target != "DELETE /v1/invites/{token}" {
				t.Fatalf("target = %q, want the route pattern (redacted)", target)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no audit row recorded for invite revoke")
	}
}

// TestAuditRetentionZeroKeepsForever checks that audit_retention_days=0
// disables the opportunistic prune the middleware runs after every append.
func TestAuditRetentionZeroKeepsForever(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "audit-retain@b.co", "admin")

	if err := st.SetSetting("audit_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO audit_log (created_at, user_email, action, target) VALUES (datetime('now', '-1000 days'), 'ancient@b.co', 'act', 't')`,
	); err != nil {
		t.Fatal(err)
	}

	// Any mutating request triggers the opportunistic prune pass.
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"retainproj"}`).Body.Close()

	list, err := st.ListAudit(0, 0, "ancient@b.co", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ancient row pruned despite audit_retention_days=0: %+v", list)
	}
}

// TestAuditAPIAdminOnly checks GET /v1/audit's admin gate and the 200-row cap.
func TestAuditAPIAdminOnly(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "audit-api-admin@b.co", "admin")
	member := seedUserToken(t, st, "audit-api-member@b.co", "member")

	forbidden := doAuthed(t, "GET", srv.URL+"/v1/audit", member, "")
	forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("member: want 403, got %d", forbidden.StatusCode)
	}

	for i := 0; i < 205; i++ {
		if err := st.AppendAudit("seed@b.co", "act", "t"); err != nil {
			t.Fatal(err)
		}
	}

	resp := doAuthed(t, "GET", srv.URL+"/v1/audit?limit=500", admin, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("admin: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Entries []struct {
			ID        int64  `json:"id"`
			UserEmail string `json:"user_email"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 200 {
		t.Fatalf("entries = %d, want capped at 200", len(out.Entries))
	}
}

func TestAuditAPIEntriesNeverNull(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "audit-empty@b.co", "admin")
	resp := doAuthed(t, "GET", srv.URL+"/v1/audit", admin, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"entries":null`) {
		t.Fatalf("entries must never be null: %s", body)
	}
}

// TestUIAuditPageAdminOnly checks /ui/audit renders rows for an admin and
// 404s a member, matching /ui/users's non-admin policy.
func TestUIAuditPageAdminOnly(t *testing.T) {
	srv, st := testServer(t)
	seedUserToken(t, st, "audit-uipage-admin@b.co", "admin")
	seedUserToken(t, st, "audit-uipage-member@b.co", "member")
	adminUser, err := st.GetUserByEmail("audit-uipage-admin@b.co")
	if err != nil {
		t.Fatal(err)
	}
	memberUser, err := st.GetUserByEmail("audit-uipage-member@b.co")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit("someone@b.co", "POST /v1/projects", "/v1/projects"); err != nil {
		t.Fatal(err)
	}

	client := noRedirectClient()

	adminReq, err := http.NewRequest("GET", srv.URL+"/ui/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	adminReq.AddCookie(uiSessionCookie(t, st, adminUser.ID))
	adminResp, err := client.Do(adminReq)
	if err != nil {
		t.Fatal(err)
	}
	defer adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin: want 200, got %d", adminResp.StatusCode)
	}
	body, err := io.ReadAll(adminResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "someone@b.co") {
		t.Fatalf("admin audit page missing row: %s", body)
	}

	memberReq, err := http.NewRequest("GET", srv.URL+"/ui/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	memberReq.AddCookie(uiSessionCookie(t, st, memberUser.ID))
	memberResp, err := client.Do(memberReq)
	if err != nil {
		t.Fatal(err)
	}
	defer memberResp.Body.Close()
	if memberResp.StatusCode != http.StatusNotFound {
		t.Fatalf("member: want 404 (same policy as /ui/users), got %d", memberResp.StatusCode)
	}
}
