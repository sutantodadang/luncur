package server

import (
	"strings"
	"testing"
)

// TestGitTokenRoundTrip covers the git-token API: set on a git app seals the
// token at rest, a non-git app is rejected, and delete clears it.
func TestGitTokenRoundTrip(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()

	// A git-source app accepts a token.
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin,
		`{"name":"gitapp","kind":"web","port":3000,"git_url":"https://github.com/me/private"}`).Body.Close()

	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/gitapp/git-token", admin, `{"token":"ghp_secret"}`); resp.StatusCode != 204 {
		t.Fatalf("set git token: want 204, got %d", resp.StatusCode)
	}

	// Sealed at rest: raw store bytes must not contain the plaintext token.
	var raw []byte
	st.DB().QueryRow(`SELECT git_token_enc FROM apps WHERE name = 'gitapp'`).Scan(&raw)
	if len(raw) == 0 {
		t.Fatal("git token not stored")
	}
	if strings.Contains(string(raw), "ghp_secret") {
		t.Fatal("git token stored unsealed")
	}

	// Empty token is rejected.
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/gitapp/git-token", admin, `{"token":""}`); resp.StatusCode != 400 {
		t.Fatalf("empty token: want 400, got %d", resp.StatusCode)
	}

	// Delete clears it.
	if resp := doAuthed(t, "DELETE", srv.URL+"/v1/projects/web/apps/gitapp/git-token", admin, ""); resp.StatusCode != 204 {
		t.Fatalf("clear git token: want 204, got %d", resp.StatusCode)
	}
	raw = nil
	st.DB().QueryRow(`SELECT git_token_enc FROM apps WHERE name = 'gitapp'`).Scan(&raw)
	if len(raw) != 0 {
		t.Fatalf("git token not cleared: %v", raw)
	}

	// A non-git (tarball) app rejects a token.
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"tar","port":8080}`).Body.Close()
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/tar/git-token", admin, `{"token":"x"}`); resp.StatusCode != 400 {
		t.Fatalf("git token on non-git app: want 400, got %d", resp.StatusCode)
	}
}
