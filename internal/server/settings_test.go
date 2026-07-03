package server

import (
	"encoding/json"
	"testing"
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
