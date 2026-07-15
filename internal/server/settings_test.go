package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/secret"
)

// TestSettingsAdminRoundTrip exercises the install-settings API as an admin:
// unset key 404s, PUT persists, GET reads it back.
func TestSettingsAdminRoundTrip(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/settings/cert_provider", admin, "")
	if resp.StatusCode != 404 {
		t.Fatalf("get unset: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/cert_provider", admin, `{"value":"traefik"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/cert_provider", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Key != "cert_provider" || out.Value != "traefik" {
		t.Fatalf("got %+v, want cert_provider/traefik", out)
	}
}

func TestSettingsMemberForbidden(t *testing.T) {
	srv, st := testServer(t)
	member := seedUserToken(t, st, "pleb@b.co", "member")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/cert_provider", member, `{"value":"builtin"}`)
	if resp.StatusCode != 403 {
		t.Fatalf("put: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/cert_provider", member, "")
	if resp.StatusCode != 403 {
		t.Fatalf("get: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSettingsTrainGangTimeout covers train_gang_timeout_minutes' entry in
// settableKeys: settable via the generic settings API, integer >= 0 only.
func TestSettingsTrainGangTimeout(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/train_gang_timeout_minutes", admin, `{"value":"15"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/train_gang_timeout_minutes", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "15" {
		t.Fatalf("value = %q, want 15", out.Value)
	}

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/train_gang_timeout_minutes", admin, `{"value":"-1"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put negative: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSettingsPreviewTTLDays covers preview_ttl_days' entry in
// settableKeys: settable via the generic settings API, integer >= 1 only —
// reapPreviews (preview.go) reads it back via previewTTLDays.
func TestSettingsPreviewTTLDays(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/preview_ttl_days", admin, `{"value":"3"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/preview_ttl_days", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "3" {
		t.Fatalf("value = %q, want 3", out.Value)
	}

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/preview_ttl_days", admin, `{"value":"0"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put zero: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsUnknownKey(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/bogus_key", admin, `{"value":"x"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put unknown: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/bogus_key", admin, "")
	if resp.StatusCode != 400 {
		t.Fatalf("get unknown: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsInvalidCertProviderValue(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/cert_provider", admin, `{"value":"bogus"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put bad value: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsBackupSchedule(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/backup_schedule", admin, `{"value":"weekly"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put weekly: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/backup_schedule", admin, `{"value":"daily"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put daily: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsBackupKeep(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/backup_keep", admin, `{"value":"abc"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put abc: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/backup_keep", admin, `{"value":"7"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put 7: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSettingsBackupS3SecretKey exercises the write-only sealed setting:
// no sealer configured -> 503; with a sealer, PUT persists a sealed value
// (never the plaintext) and GET echoes "(set)" instead of the secret.
func TestSettingsRegistryKeep(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/registry_keep", admin, `{"value":"0"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put 0: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/registry_keep", admin, `{"value":"10"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put 10: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsBuildTimeoutMinutes(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	for _, bad := range []string{"0", "800", "abc"} {
		resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/build_timeout_minutes", admin, `{"value":"`+bad+`"}`)
		if resp.StatusCode != 400 {
			t.Fatalf("put %q: want 400, got %d", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/build_timeout_minutes", admin, `{"value":"30"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put 30: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSettingsBuildCache(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/build_cache", admin, `{"value":"bogus"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put bogus: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/build_cache", admin, `{"value":"off"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put off: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/build_cache", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "off" {
		t.Fatalf("build_cache = %q, want off", out.Value)
	}
}

func TestSettingsBackupS3SecretKey(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/backup_s3_secret_key", admin, `{"value":"topsecret"}`)
	if resp.StatusCode != 503 {
		t.Fatalf("put without sealer: want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st2 := newTestStore(t)
	srv2 := newHTTPTest(t, Deps{Store: st2, Sealer: sealer})
	admin2 := seedUserToken(t, st2, "root@b.co", "admin")

	resp = doAuthed(t, "PUT", srv2.URL+"/v1/settings/backup_s3_secret_key", admin2, `{"value":"topsecret"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put with sealer: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv2.URL+"/v1/settings/backup_s3_secret_key", admin2, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "(set)" {
		t.Fatalf("get value = %q, want (set)", out.Value)
	}

	raw, err := st2.GetSetting("backup_s3_secret_key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "sealed:") {
		t.Fatalf("raw setting = %q, want sealed: prefix", raw)
	}
}

// TestSettingsSMTPKeys: plain smtp_* keys round-trip; smtp_port must be a
// valid port number.
func TestSettingsSMTPKeys(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_host", admin, `{"value":"mail.example.com"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put smtp_host: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_port", admin, `{"value":"70000"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put smtp_port 70000: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_port", admin, `{"value":"587"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put smtp_port 587: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/smtp_host", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get smtp_host: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "mail.example.com" {
		t.Fatalf("smtp_host = %q, want mail.example.com", out.Value)
	}
}

// TestSettingsSMTPPassSealed mirrors TestSettingsBackupS3SecretKey for
// smtp_pass: 503 without a sealer; sealed at rest; GET masks to "(set)".
func TestSettingsSMTPPassSealed(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_pass", admin, `{"value":"hunter2"}`)
	if resp.StatusCode != 503 {
		t.Fatalf("put without sealer: want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st2 := newTestStore(t)
	srv2 := newHTTPTest(t, Deps{Store: st2, Sealer: sealer})
	admin2 := seedUserToken(t, st2, "root@b.co", "admin")

	resp = doAuthed(t, "PUT", srv2.URL+"/v1/settings/smtp_pass", admin2, `{"value":"hunter2"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put with sealer: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv2.URL+"/v1/settings/smtp_pass", admin2, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "(set)" {
		t.Fatalf("get value = %q, want (set)", out.Value)
	}

	raw, err := st2.GetSetting("smtp_pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "sealed:") {
		t.Fatalf("raw smtp_pass = %q, want sealed: prefix", raw)
	}
}

// TestSettingsDNSKeys: provider enum enforced; a sealed dns cred masks.
func TestSettingsDNSKeys(t *testing.T) {
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"gandi"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad provider: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, v := range []string{"cloudflare", "route53", "rfc2136", "desec", "hetzner", "digitalocean", "none"} {
		resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_provider", admin, `{"value":"`+v+`"}`)
		if resp.StatusCode != 204 {
			t.Fatalf("provider %s: want 204, got %d", v, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/dns_cloudflare_token", admin, `{"value":"tok"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put token: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/dns_cloudflare_token", admin, "")
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "(set)" {
		t.Fatalf("token read = %q, want (set)", out.Value)
	}

	// desec/hetzner/digitalocean tokens are sealed the same way as the
	// cloudflare token above.
	for _, key := range []string{"dns_desec_token", "dns_hetzner_token", "dns_digitalocean_token"} {
		resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/"+key, admin, `{"value":"tok"}`)
		if resp.StatusCode != 204 {
			t.Fatalf("put %s: want 204, got %d", key, resp.StatusCode)
		}
		resp.Body.Close()

		resp = doAuthed(t, "GET", srv.URL+"/v1/settings/"+key, admin, "")
		var tokOut struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tokOut); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if tokOut.Value != "(set)" {
			t.Fatalf("%s read = %q, want (set)", key, tokOut.Value)
		}

		raw, err := st.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(raw, "sealed:") {
			t.Fatalf("raw %s = %q, want sealed: prefix", key, raw)
		}
	}
}

// TestSettingsNotifyURLSealed mirrors TestSettingsSMTPPassSealed for
// notify_url: 503 without a sealer; sealed at rest; GET masks to "(set)".
func TestSettingsNotifyURLSealed(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_url", admin, `{"value":"https://hooks.example.com/x"}`)
	if resp.StatusCode != 503 {
		t.Fatalf("put without sealer: want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st2 := newTestStore(t)
	srv2 := newHTTPTest(t, Deps{Store: st2, Sealer: sealer})
	admin2 := seedUserToken(t, st2, "root@b.co", "admin")

	resp = doAuthed(t, "PUT", srv2.URL+"/v1/settings/notify_url", admin2, `{"value":"https://hooks.example.com/x"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put with sealer: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv2.URL+"/v1/settings/notify_url", admin2, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "(set)" {
		t.Fatalf("get value = %q, want (set)", out.Value)
	}

	raw, err := st2.GetSetting("notify_url")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "sealed:") {
		t.Fatalf("raw notify_url = %q, want sealed: prefix", raw)
	}
}

// TestSettingsNotifyFormatAndEvents: notify_format enum enforced;
// notify_events validated as a CSV subset of known event names.
func TestSettingsNotifyFormatAndEvents(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_format", admin, `{"value":"bogus"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad format: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, v := range []string{"generic", "discord", "slack", "telegram", "email"} {
		resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_format", admin, `{"value":"`+v+`"}`)
		if resp.StatusCode != 204 {
			t.Fatalf("format %s: want 204, got %d", v, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_events", admin, `{"value":"deploy_failed,bogus_event"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad events: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_events", admin, `{"value":""}`)
	if resp.StatusCode != 400 {
		t.Fatalf("empty events: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_events", admin, `{"value":"deploy_success, cert_failed"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("valid events: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/notify_events", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "deploy_success, cert_failed" {
		t.Fatalf("notify_events roundtrip = %q", out.Value)
	}

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_telegram_chat", admin, `{"value":"123456"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put telegram chat: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSettingsNotifyEmailValidation: notify_email must be a non-empty CSV
// of addresses that each look like an email address; it is a plain
// (non-sealed) setting, unlike notify_url.
func TestSettingsNotifyEmailValidation(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	for _, bad := range []string{"", "not-an-email", "a@b.co,not-an-email", "a@b.co,"} {
		resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_email", admin, `{"value":"`+bad+`"}`)
		if resp.StatusCode != 400 {
			t.Fatalf("notify_email %q: want 400, got %d", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/notify_email", admin, `{"value":"a@b.co, c@d.co"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("valid notify_email: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/notify_email", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "a@b.co, c@d.co" {
		t.Fatalf("notify_email roundtrip = %q (not sealed, should echo)", out.Value)
	}
}
