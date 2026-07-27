package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestAuditToCLIMappings covers auditToCLI's route table (DESIGN.md
// "Session Transcript"): every UI mutation the task requires the Session
// Transcript to translate, plus two routes that must stay unmapped (skipped
// by the transcript, never rendered raw).
func TestAuditToCLIMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method, pattern, target string
		want                    string
		wantOK                  bool
	}{
		{"POST", "/ui/projects/{project}/apps/{app}/deploy", "/ui/projects/p1/apps/api/deploy", "luncur deploy api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/redeploy", "/ui/projects/p1/apps/api/redeploy", "luncur redeploy api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/rollback", "/ui/projects/p1/apps/api/rollback", "luncur rollback api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/scale", "/ui/projects/p1/apps/api/scale", "luncur scale api <n> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/autoscale", "/ui/projects/p1/apps/api/autoscale", "luncur autoscale api --min <n> --max <n> --cpu <n> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/env", "/ui/projects/p1/apps/api/env", "luncur env set api <KEY=VALUE> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/env/bulk", "/ui/projects/p1/apps/api/env/bulk", "luncur env push api <file> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/env/delete", "/ui/projects/p1/apps/api/env/delete", "luncur env unset api <key> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/domains", "/ui/projects/p1/apps/api/domains", "luncur domain add api <hostname> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/domains/delete", "/ui/projects/p1/apps/api/domains/delete", "luncur domain remove api <hostname> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/domains/retry", "/ui/projects/p1/apps/api/domains/retry", "luncur domain retry api <hostname> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/volumes", "/ui/projects/p1/apps/api/volumes", "luncur volume add api <path> --size <n> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/volumes/remove", "/ui/projects/p1/apps/api/volumes/remove", "luncur volume remove api <name> --project p1", true},
		{"POST", "/ui/projects/{project}/addons", "/ui/projects/p1/addons", "luncur addon create <type> --project p1", true},
		{"POST", "/ui/projects/{project}/addons/delete", "/ui/projects/p1/addons/delete", "luncur addon remove <name> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/addons/attach", "/ui/projects/p1/apps/api/addons/attach", "luncur addon attach <name> api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/addons/detach", "/ui/projects/p1/apps/api/addons/detach", "luncur addon detach <name> api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/git-token", "/ui/projects/p1/apps/api/git-token", "luncur app git-token set api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/git-token/clear", "/ui/projects/p1/apps/api/git-token/clear", "luncur app git-token clear api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/webhook", "/ui/projects/p1/apps/api/webhook", "luncur webhook enable api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/webhook/disable", "/ui/projects/p1/apps/api/webhook/disable", "luncur webhook disable api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/pause", "/ui/projects/p1/apps/api/pause", "luncur app pause api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/resume", "/ui/projects/p1/apps/api/resume", "luncur app resume api --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/run-now", "/ui/projects/p1/apps/api/run-now", "luncur app run-now api --project p1", true},
		{"POST", "/ui/projects/{project}/apps", "/ui/projects/p1/apps", "luncur app create <name> --project p1", true},
		{"POST", "/ui/projects/{project}/apps/{app}/destroy", "/ui/projects/p1/apps/api/destroy", "luncur destroy api --project p1", true},
		{"POST", "/ui/projects", "/ui/projects", "luncur project create <name>", true},
		{"POST", "/ui/projects/{project}/rename", "/ui/projects/p1/rename", "luncur project rename p1 <new-name>", true},
		// Unmappable: skipped by the transcript, not rendered raw.
		{"POST", "/ui/settings", "/ui/settings", "", false},
		{"POST", "/ui/projects/{project}/apps/{app}/eject", "/ui/projects/p1/apps/api/eject", "", false},
	}
	for _, tc := range cases {
		vars := pathVarsFrom(tc.pattern, tc.target)
		got, ok := auditToCLI(tc.method, tc.pattern, vars)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("auditToCLI(%q, %q) = (%q, %v), want (%q, %v)", tc.method, tc.pattern, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestTickerSummary covers the event ticker's humanizer: mapped routes
// summarize via auditToCLI, unmapped routes fall back to a generic
// last-segment verb, still folding in the app/project var when present.
func TestTickerSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		action, target, want string
	}{
		{"POST /ui/projects/{project}/apps/{app}/deploy", "/ui/projects/p1/apps/api/deploy", "deploy api"},
		{"POST /ui/projects/{project}/apps/{app}/scale", "/ui/projects/p1/apps/api/scale", "scale api <n>"},
		{"POST /ui/users/invite", "/ui/users/invite", "invite"},
		{"POST /ui/projects/{project}/apps/{app}/eject", "/ui/projects/p1/apps/api/eject", "eject api"},
	}
	for _, tc := range cases {
		if got := tickerSummary(tc.action, tc.target); got != tc.want {
			t.Errorf("tickerSummary(%q, %q) = %q, want %q", tc.action, tc.target, got, tc.want)
		}
	}
}

// TestUITickerScoping is the SECURITY assertion for GET /ui/ticker: a
// non-admin only ever sees their own latest action, even when another
// user's action is more recent overall; an admin sees the true latest row
// regardless of author.
func TestUITickerScoping(t *testing.T) {
	t.Parallel()
	srv, st := testServer(t)
	seedUserToken(t, st, "ticker-member@b.co", "member")
	seedUserToken(t, st, "ticker-admin@b.co", "admin")
	member, err := st.GetUserByEmail("ticker-member@b.co")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := st.GetUserByEmail("ticker-admin@b.co")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.AppendAudit("ticker-member@b.co", "POST /ui/projects/{project}/apps/{app}/deploy", "/ui/projects/p1/apps/memberapp/deploy"); err != nil {
		t.Fatal(err)
	}
	// A different user's action, appended after — the global latest row.
	if err := st.AppendAudit("ticker-other@b.co", "POST /ui/projects/{project}/apps/{app}/scale", "/ui/projects/p1/apps/otherapp/scale"); err != nil {
		t.Fatal(err)
	}

	client := noRedirectClient()

	memberStatus, memberBody := getUIPage(t, client, srv.URL, "/ui/ticker", uiSessionCookie(t, st, member.ID))
	if memberStatus != http.StatusOK {
		t.Fatalf("member ticker: want 200, got %d", memberStatus)
	}
	if !strings.Contains(memberBody, "memberapp") {
		t.Fatalf("member ticker missing own latest action: %s", memberBody)
	}
	if strings.Contains(memberBody, "otherapp") {
		t.Fatalf("SECURITY: member ticker leaked another user's action: %s", memberBody)
	}

	adminStatus, adminBody := getUIPage(t, client, srv.URL, "/ui/ticker", uiSessionCookie(t, st, admin.ID))
	if adminStatus != http.StatusOK {
		t.Fatalf("admin ticker: want 200, got %d", adminStatus)
	}
	if !strings.Contains(adminBody, "otherapp") {
		t.Fatalf("admin ticker should see the global latest action: %s", adminBody)
	}
}

// TestUITranscriptRendersChronologicalAndSkipsUnmappable checks the
// transcript fragment: mapped rows render oldest-first (script order),
// unmappable rows are skipped entirely rather than shown raw.
func TestUITranscriptRendersChronologicalAndSkipsUnmappable(t *testing.T) {
	t.Parallel()
	srv, st := testServer(t)
	seedUserToken(t, st, "transcript-member@b.co", "member")
	u, err := st.GetUserByEmail("transcript-member@b.co")
	if err != nil {
		t.Fatal(err)
	}

	// Oldest first: deploy, then an unmappable settings change, then scale.
	if err := st.AppendAudit("transcript-member@b.co", "POST /ui/projects/{project}/apps/{app}/deploy", "/ui/projects/p1/apps/appA/deploy"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit("transcript-member@b.co", "POST /ui/settings", "/ui/settings"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit("transcript-member@b.co", "POST /ui/projects/{project}/apps/{app}/scale", "/ui/projects/p1/apps/appB/scale"); err != nil {
		t.Fatal(err)
	}

	client := noRedirectClient()
	status, body := getUIPage(t, client, srv.URL, "/ui/transcript", uiSessionCookie(t, st, u.ID))
	if status != http.StatusOK {
		t.Fatalf("transcript: want 200, got %d", status)
	}

	// html/template escapes "<n>" to "&lt;n&gt;" in the rendered <pre> body
	// (the DOM's actual textContent — what the copy button reads — decodes
	// back to the literal placeholder; only the raw HTTP response seen here
	// is escaped).
	wantDeploy := "luncur deploy appA --project p1"
	wantScale := "luncur scale appB &lt;n&gt; --project p1"
	if !strings.Contains(body, wantDeploy) || !strings.Contains(body, wantScale) {
		t.Fatalf("transcript missing mapped commands: %s", body)
	}
	if strings.Index(body, wantDeploy) > strings.Index(body, wantScale) {
		t.Fatalf("transcript not chronological (deploy should render before scale): %s", body)
	}
	if strings.Count(body, "luncur ") != 2 {
		t.Fatalf("transcript should skip the unmappable /ui/settings row, got: %s", body)
	}
}

// TestUITranscriptScopedToOwnEmail is the SECURITY assertion for GET
// /ui/transcript: it must never surface another user's audit rows, admin or
// not — the transcript is a personal runbook, not an admin audit view.
func TestUITranscriptScopedToOwnEmail(t *testing.T) {
	t.Parallel()
	srv, st := testServer(t)
	seedUserToken(t, st, "transcript-member2@b.co", "member")
	u, err := st.GetUserByEmail("transcript-member2@b.co")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.AppendAudit("transcript-member2@b.co", "POST /ui/projects/{project}/apps/{app}/deploy", "/ui/projects/p1/apps/mine/deploy"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit("transcript-other2@b.co", "POST /ui/projects/{project}/apps/{app}/deploy", "/ui/projects/p1/apps/notmine/deploy"); err != nil {
		t.Fatal(err)
	}

	client := noRedirectClient()
	status, body := getUIPage(t, client, srv.URL, "/ui/transcript", uiSessionCookie(t, st, u.ID))
	if status != http.StatusOK {
		t.Fatalf("transcript: want 200, got %d", status)
	}
	if !strings.Contains(body, "mine") {
		t.Fatalf("transcript missing own action: %s", body)
	}
	if strings.Contains(body, "notmine") {
		t.Fatalf("SECURITY: transcript leaked another user's action: %s", body)
	}
}

// TestUITranscriptAbsentFromNonAppPages checks the Session Transcript panel
// only renders on the app page (app.html), not on other authenticated pages
// like the project's apps list.
func TestUITranscriptAbsentFromNonAppPages(t *testing.T) {
	t.Parallel()
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "transcript-nonapp@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	u, err := st.GetUserByEmail("transcript-nonapp@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	_, listBody := getUIPage(t, client, srv.URL, "/ui/projects/proj", ck)
	if strings.Contains(strings.ToLower(listBody), "session transcript") {
		t.Fatalf("projects list page should not render the Session Transcript panel: %s", listBody)
	}

	_, appBody := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if !strings.Contains(strings.ToLower(appBody), "session transcript") {
		t.Fatalf("app page missing the Session Transcript panel: %s", appBody)
	}
}
