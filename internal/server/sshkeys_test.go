package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHPubKey generates a fresh ed25519 key in authorized_keys format.
func testSSHPubKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestSSHKeyEndpoints(t *testing.T) {
	srv, st := testServer(t)
	tok := seedUserToken(t, st, "dev@b.co", "member")
	pub := testSSHPubKey(t)

	// 1. POST /v1/ssh-keys → 201, body contains "SHA256:".
	body := fmt.Sprintf(`{"name":"laptop","public_key":%q}`, pub)
	created := doAuthed(t, "POST", srv.URL+"/v1/ssh-keys", tok, body)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", created.StatusCode)
	}
	var createdOut struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(created.Body).Decode(&createdOut); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(createdOut.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q, want SHA256:...", createdOut.Fingerprint)
	}

	// 2. GET /v1/ssh-keys → 200 with one entry, name "laptop", fingerprint present.
	list := doAuthed(t, "GET", srv.URL+"/v1/ssh-keys", tok, "")
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200, got %d", list.StatusCode)
	}
	var keys []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
		CreatedAt   string `json:"created_at"`
	}
	if err := json.NewDecoder(list.Body).Decode(&keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "laptop" || keys[0].Fingerprint == "" {
		t.Fatalf("list = %+v", keys)
	}

	// 3. DELETE /v1/ssh-keys/{id} → 204.
	del := doAuthed(t, "DELETE", fmt.Sprintf("%s/v1/ssh-keys/%d", srv.URL, keys[0].ID), tok, "")
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", del.StatusCode)
	}

	// 4. GET → empty list.
	empty := doAuthed(t, "GET", srv.URL+"/v1/ssh-keys", tok, "")
	defer empty.Body.Close()
	if empty.StatusCode != http.StatusOK {
		t.Fatalf("list after delete: want 200, got %d", empty.StatusCode)
	}
	var emptyKeys []map[string]any
	if err := json.NewDecoder(empty.Body).Decode(&emptyKeys); err != nil {
		t.Fatal(err)
	}
	if len(emptyKeys) != 0 {
		t.Fatalf("want empty list, got %+v", emptyKeys)
	}

	// 5. POST with public_key "garbage" → 400, code bad_request.
	bad := doAuthed(t, "POST", srv.URL+"/v1/ssh-keys", tok, `{"name":"bad","public_key":"garbage"}`)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage: want 400, got %d", bad.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bad.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "bad_request" {
		t.Fatalf("want code bad_request, got %q", env.Error.Code)
	}
}
