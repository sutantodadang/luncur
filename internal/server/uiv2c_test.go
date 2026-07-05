package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// settingsTestServer builds a server with a sealer wired up (needed for the
// sealed-key round trip), matching backupServer's Deps shape minus the data
// dir (settings tests don't need backups).
func settingsTestServer(t *testing.T) (*httptestServer, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer})
	return srv, st
}

// TestUISettingsAdminOnly checks the page 404s for a member (matching
// handleUIUsers' leak-nothing behavior) and 200s with a known key's form
// for an admin.
func TestUISettingsAdminOnly(t *testing.T) {
	srv, st := settingsTestServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, _ := getUIPage(t, client, srv.URL, "/ui/settings", uiSessionCookie(t, st, member.ID))
	if status != http.StatusNotFound {
		t.Fatalf("member GET /ui/settings: want 404, got %d", status)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/settings", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/settings: want 200, got %d", status)
	}
	if !strings.Contains(body, `name="key" value="cert_provider"`) {
		t.Fatalf("settings page missing cert_provider form, got: %s", body)
	}
}

// TestUISettingsSetValid POSTs a valid plain key and asserts the store was
// updated and the redirect carries a saved banner.
func TestUISettingsSetValid(t *testing.T) {
	srv, st := settingsTestServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/settings", csrfCk, adminCk,
		url.Values{"key": {"cert_provider"}, "value": {"traefik"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post valid setting: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/settings?saved=cert_provider" {
		t.Fatalf("Location = %q, want saved banner", loc)
	}
	v, err := st.GetSetting("cert_provider")
	if err != nil || v != "traefik" {
		t.Fatalf("GetSetting cert_provider = %q, err=%v, want traefik", v, err)
	}
}

// TestUISettingsSealedKeyShowsSet POSTs a sealed key's value and asserts the
// stored value is sealed (never plaintext) and the page shows "(set)"
// instead of echoing it.
func TestUISettingsSealedKeyShowsSet(t *testing.T) {
	srv, st := settingsTestServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/settings", csrfCk, adminCk,
		url.Values{"key": {"smtp_pass"}, "value": {"supersecret"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post sealed setting: want 303, got %d", resp.StatusCode)
	}

	stored, err := st.GetSetting("smtp_pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "sealed:") || strings.Contains(stored, "supersecret") {
		t.Fatalf("stored smtp_pass = %q, want sealed and never plaintext", stored)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/settings", adminCk)
	if status != http.StatusOK {
		t.Fatalf("GET settings: want 200, got %d", status)
	}
	if !strings.Contains(body, "(set)") {
		t.Fatalf("settings page should show (set) for smtp_pass, got: %s", body)
	}
	if strings.Contains(body, "supersecret") {
		t.Fatalf("settings page leaked sealed plaintext: %s", body)
	}
}

// TestUISettingsInvalidValue POSTs an invalid enum value and asserts the
// store is left unchanged and an error is surfaced via redirect.
func TestUISettingsInvalidValue(t *testing.T) {
	srv, st := settingsTestServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/settings", csrfCk, adminCk,
		url.Values{"key": {"cert_provider"}, "value": {"bogus"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post invalid value: want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/ui/settings?err=") {
		t.Fatalf("Location = %q, want an error redirect", loc)
	}
	if _, err := st.GetSetting("cert_provider"); err == nil {
		t.Fatal("cert_provider should remain unset after an invalid value")
	}
}

// TestUISettingsUnknownKey POSTs an unrecognized key and asserts 400.
func TestUISettingsUnknownKey(t *testing.T) {
	srv, st := settingsTestServer(t)
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/settings", csrfCk, adminCk,
		url.Values{"key": {"bogus_key"}, "value": {"x"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("post unknown key: want 400, got %d", resp.StatusCode)
	}
}

// TestUIRegistryGCAdminGated checks the route is admin-only and, given a
// reachable (empty) fake registry, redirects back to /ui/settings with a
// result banner even without kube configured (the manifest-delete phase
// runs standalone; the exec/blob-reclaim phase just downgrades to a
// warning per runRegistryGC's contract).
func TestUIRegistryGCAdminGated(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	csrfCk := uiCSRF(t, client, srv.URL)

	memberResp := uiPost(t, client, srv.URL+"/ui/registry-gc", csrfCk, uiSessionCookie(t, st, member.ID), url.Values{})
	memberResp.Body.Close()
	if memberResp.StatusCode != http.StatusNotFound {
		t.Fatalf("member registry-gc: want 404, got %d", memberResp.StatusCode)
	}

	adminResp := uiPost(t, client, srv.URL+"/ui/registry-gc", csrfCk, uiSessionCookie(t, st, admin.ID), url.Values{})
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin registry-gc: want 303, got %d", adminResp.StatusCode)
	}
	if loc := adminResp.Header.Get("Location"); !strings.HasPrefix(loc, "/ui/settings?gc=") {
		t.Fatalf("Location = %q, want a gc banner redirect", loc)
	}
}

// TestUIBackupsPage seeds one backup row directly in the store and checks
// the page lists it, and that create/prune are admin-gated.
func TestUIBackupsPage(t *testing.T) {
	st := newTestStore(t)
	dataDir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "luncur.key")
	if err := os.WriteFile(keyPath, []byte("KEYBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer, DataDir: dataDir, SecretKeyPath: keyPath})

	if _, err := st.CreateBackup(filepath.Join(dataDir, "luncur-20260101-000000.tar.gz"), 500, false); err != nil {
		t.Fatal(err)
	}

	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)

	status, body := getUIPage(t, client, srv.URL, "/ui/backups", adminCk)
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/backups: want 200, got %d", status)
	}
	if !strings.Contains(body, "luncur-20260101-000000.tar.gz") || !strings.Contains(body, "500 B") {
		t.Fatalf("backups page missing seeded row, got: %s", body)
	}

	status, _ = getUIPage(t, client, srv.URL, "/ui/backups", uiSessionCookie(t, st, member.ID))
	if status != http.StatusNotFound {
		t.Fatalf("member GET /ui/backups: want 404, got %d", status)
	}

	csrfCk := uiCSRF(t, client, srv.URL)
	createResp := uiPost(t, client, srv.URL+"/ui/backups", csrfCk, adminCk, url.Values{})
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin create backup: want 303, got %d", createResp.StatusCode)
	}
	pruneResp := uiPost(t, client, srv.URL+"/ui/backups/prune", csrfCk, adminCk, url.Values{})
	pruneResp.Body.Close()
	if pruneResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin prune backups: want 303, got %d", pruneResp.StatusCode)
	}

	memberCreate := uiPost(t, client, srv.URL+"/ui/backups", csrfCk, uiSessionCookie(t, st, member.ID), url.Values{})
	memberCreate.Body.Close()
	if memberCreate.StatusCode != http.StatusNotFound {
		t.Fatalf("member create backup: want 404, got %d", memberCreate.StatusCode)
	}
}

// TestUISSHKeysAddAndDelete adds a key via the UI form, checks it's listed,
// deletes it, checks it's gone, and checks the page requires login.
func TestUISSHKeysAddAndDelete(t *testing.T) {
	srv, st := testServer(t)
	client := noRedirectClient()

	status, _ := getUIPage(t, client, srv.URL, "/ui/sshkeys", nil)
	if status != http.StatusSeeOther {
		t.Fatalf("no session: want 303 to login, got %d", status)
	}

	u, err := st.CreateUser("dev@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	csrfCk := uiCSRF(t, client, srv.URL)
	pub := testSSHPubKey(t)

	addResp := uiPost(t, client, srv.URL+"/ui/sshkeys", csrfCk, ck,
		url.Values{"name": {"laptop"}, "public_key": {pub}})
	addResp.Body.Close()
	if addResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add key: want 303, got %d", addResp.StatusCode)
	}

	status, body := getUIPage(t, client, srv.URL, "/ui/sshkeys", ck)
	if status != http.StatusOK {
		t.Fatalf("GET sshkeys: want 200, got %d", status)
	}
	if !strings.Contains(body, "laptop") {
		t.Fatalf("sshkeys page missing added key, got: %s", body)
	}

	keys, err := st.ListSSHKeys(u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %+v, err=%v", keys, err)
	}

	delResp := uiPost(t, client, srv.URL+"/ui/sshkeys/delete", csrfCk, ck,
		url.Values{"id": {strconv.FormatInt(keys[0].ID, 10)}})
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete key: want 303, got %d", delResp.StatusCode)
	}

	status, body = getUIPage(t, client, srv.URL, "/ui/sshkeys", ck)
	if status != http.StatusOK {
		t.Fatalf("GET sshkeys after delete: want 200, got %d", status)
	}
	if strings.Contains(body, "laptop") {
		t.Fatalf("sshkeys page should not list deleted key, got: %s", body)
	}
}

// TestUIDoctorPage checks the admin sees all 9 check names plus the server
// version, and a member gets 404.
func TestUIDoctorPage(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost, Version: "v9.9.9"})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/doctor", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/doctor: want 200, got %d", status)
	}
	if !strings.Contains(body, "v9.9.9") {
		t.Fatalf("doctor page missing version, got: %s", body)
	}
	for _, name := range []string{"database", "kubernetes", "registry", "builds", "ingress",
		"certificates", "smtp", "notifications", "backups"} {
		if !strings.Contains(body, name) {
			t.Fatalf("doctor page missing check %q, got: %s", name, body)
		}
	}

	status, _ = getUIPage(t, client, srv.URL, "/ui/doctor", uiSessionCookie(t, st, member.ID))
	if status != http.StatusNotFound {
		t.Fatalf("member GET /ui/doctor: want 404, got %d", status)
	}
}

// TestUINavAdminLinks checks the sidebar shows /ui/settings for an admin but
// not for a member.
func TestUINavAdminLinks(t *testing.T) {
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

	status, body := getUIPage(t, client, srv.URL, "/ui/", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/: want 200, got %d", status)
	}
	if !strings.Contains(body, `href="/ui/settings"`) {
		t.Fatalf("admin nav missing /ui/settings link, got: %s", body)
	}

	status, body = getUIPage(t, client, srv.URL, "/ui/", uiSessionCookie(t, st, member.ID))
	if status != http.StatusOK {
		t.Fatalf("member GET /ui/: want 200, got %d", status)
	}
	if strings.Contains(body, `href="/ui/settings"`) {
		t.Fatalf("member nav should not show /ui/settings link, got: %s", body)
	}
}
