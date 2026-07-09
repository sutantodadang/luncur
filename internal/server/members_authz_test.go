package server

import (
	"encoding/json"
	"testing"
)

// TestViewerIsReadOnly: an admin creates a project and an app; a viewer
// member (role "viewer") can read (GET) but any mutating route rejects them
// with 403 read_only, per requireProjectWrite.
func TestViewerIsReadOnly(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "admin@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create project: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":3000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create app: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	viewerTok := seedUserToken(t, st, "viewer@b.co", "member")
	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := st.GetUserByEmail("viewer@b.co")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(p.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}

	// GET is allowed for a viewer.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/proj/apps", viewerTok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("viewer GET apps: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// POST (mutating) is rejected for a viewer.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", viewerTok, `{"name":"api2","port":3001}`)
	if resp.StatusCode != 403 {
		t.Fatalf("viewer POST apps: want 403, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if body["error"] != "read_only" {
		t.Fatalf("error code: got %v, want read_only", body["error"])
	}
}
