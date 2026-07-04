package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestUIWebhookRowGating checks the app page shows no Webhook section for a
// tarball-source app, but does for a git-source app.
func TestUIWebhookRowGating(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"tb","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	getPage := func(path string) string {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+path, nil)
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
			t.Fatalf("GET %s: want 200, got %d", path, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	if strings.Contains(getPage("/ui/projects/proj/apps/tb"), "<h2>Webhook</h2>") {
		t.Fatal("tarball app page should not show a Webhook section")
	}
	if !strings.Contains(getPage("/ui/projects/proj/apps/g"), "<h2>Webhook</h2>") {
		t.Fatal("git app page should show a Webhook section")
	}
}

// TestUIWebhookEnableShowsSecretOnceThenDisable exercises the UI enable
// flow: the response to the enable POST itself contains the plaintext
// secret (shown once), a reload of the app page never shows it again, and
// disable removes the webhook row's URL/disable-button state.
func TestUIWebhookEnableShowsSecretOnceThenDisable(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://x/y.git"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	enableResp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/g/webhook", csrfCk, ck, url.Values{})
	defer enableResp.Body.Close()
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("enable webhook: want 200 (rendered page, not redirect), got %d", enableResp.StatusCode)
	}
	enableBody, err := io.ReadAll(enableResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enableBody), "shown once") {
		t.Fatalf("enable response should show the secret once, got: %s", enableBody)
	}
	if !strings.Contains(string(enableBody), "/hooks/apps/proj/g") {
		t.Fatalf("enable response should show the webhook URL, got: %s", enableBody)
	}

	// Reload: the secret is gone, but the URL + disable button remain.
	req, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj/apps/g", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	reloadResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadResp.Body.Close()
	reloadBody, err := io.ReadAll(reloadResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(reloadBody), "shown once") {
		t.Fatalf("reload must not re-show the secret, got: %s", reloadBody)
	}
	if !strings.Contains(string(reloadBody), "/hooks/apps/proj/g") {
		t.Fatalf("reload should still show the webhook URL, got: %s", reloadBody)
	}
	if !strings.Contains(string(reloadBody), "disable webhook") {
		t.Fatalf("reload should show a disable button, got: %s", reloadBody)
	}

	// Disable: redirects back, and the row reverts to "enable webhook".
	disResp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/g/webhook/disable", csrfCk, ck, url.Values{})
	disResp.Body.Close()
	if disResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable webhook: want 303, got %d", disResp.StatusCode)
	}

	req2, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj/apps/g", nil)
	if err != nil {
		t.Fatal(err)
	}
	req2.AddCookie(ck)
	afterResp, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer afterResp.Body.Close()
	afterBody, err := io.ReadAll(afterResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterBody), "enable webhook") {
		t.Fatalf("after disable: want enable button, got: %s", afterBody)
	}
	if strings.Contains(string(afterBody), "disable webhook") {
		t.Fatalf("after disable: should not show disable button, got: %s", afterBody)
	}
}
