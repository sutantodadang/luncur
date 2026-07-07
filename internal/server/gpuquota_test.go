package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestGPUQuotaEndpoint(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()

	// Set quota as admin.
	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/gpu-quota", admin, `{"quota":2}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set quota: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"gpu_quota":2`) {
		t.Fatalf("body = %s", body)
	}

	// Negative rejected.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/gpu-quota", admin, `{"quota":-1}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateAppOverGPUBudget(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/gpu-quota", admin, `{"quota":1}`).Body.Close()

	// gpu=2 over budget 1 -> friendly 400 mentioning both numbers.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/p1/apps", admin, `{"name":"train","kind":"worker","gpu":2}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-budget create: %d %s", resp.StatusCode, body)
	}
	b := string(body)
	if !strings.Contains(b, "budget") || !strings.Contains(b, "1") || !strings.Contains(b, "2") {
		t.Fatalf("error not friendly: %s", b)
	}

	// Within budget passes validation and creates (DB-only, no kube needed
	// for a worker app).
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/p1/apps", admin, `{"name":"train2","kind":"worker","gpu":1}`)
	body = mustReadBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("within-budget create: %d %s", resp.StatusCode, body)
	}
}
