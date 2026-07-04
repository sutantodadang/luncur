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
