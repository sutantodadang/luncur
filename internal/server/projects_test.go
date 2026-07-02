package server

import (
	"encoding/json"
	"testing"
)

func TestProjectRoutes(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	member := seedUserToken(t, st, "m@b.co", "member")

	// Create: admin only.
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects", member, `{"name":"web"}`); resp.StatusCode != 403 {
		t.Fatalf("member create: want 403, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`); resp.StatusCode != 201 {
		t.Fatalf("admin create: want 201, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"BAD NAME"}`); resp.StatusCode != 400 {
		t.Fatalf("bad name: want 400, got %d", resp.StatusCode)
	}

	// List: member sees nothing until added.
	var list []map[string]any
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects", member, "")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("member list before membership: %v", list)
	}

	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/members", admin, `{"email":"m@b.co"}`); resp.StatusCode != 204 {
		t.Fatalf("add member: want 204, got %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/members", admin, `{"email":"ghost@b.co"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown email: want 404, got %d", resp.StatusCode)
	}

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects", member, "")
	list = nil
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0]["name"] != "web" {
		t.Fatalf("member list after membership: %v", list)
	}
}
