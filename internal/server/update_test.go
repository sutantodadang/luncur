package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSystemUpdate(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	// Valid version tag -> 202, image resolved from serverImageRepo.
	resp := doAuthed(t, "POST", srv.URL+"/v1/system/update", admin, `{"version":"v9.9.9"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid version: want 202, got %d", resp.StatusCode)
	}
	var out struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Image != "ghcr.io/sutantodadang/luncur:v9.9.9" {
		t.Fatalf("image = %q, want ghcr.io/sutantodadang/luncur:v9.9.9", out.Image)
	}

	// Invalid version tag -> 400.
	resp = doAuthed(t, "POST", srv.URL+"/v1/system/update", admin, `{"version":"not-a-tag"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid version: want 400, got %d", resp.StatusCode)
	}

	// Neither version nor image -> 400.
	resp = doAuthed(t, "POST", srv.URL+"/v1/system/update", admin, `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body: want 400, got %d", resp.StatusCode)
	}

	// Non-admin token -> 403.
	member := seedUserToken(t, st, "pleb@b.co", "member")
	resp = doAuthed(t, "POST", srv.URL+"/v1/system/update", member, `{"version":"v9.9.9"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member: want 403, got %d", resp.StatusCode)
	}

	// Explicit image wins as-is.
	resp = doAuthed(t, "POST", srv.URL+"/v1/system/update", admin, `{"image":"custom/image:v1"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("explicit image: want 202, got %d", resp.StatusCode)
	}
	out.Image = ""
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Image != "custom/image:v1" {
		t.Fatalf("image = %q, want custom/image:v1", out.Image)
	}
}
