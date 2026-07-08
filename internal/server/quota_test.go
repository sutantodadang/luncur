package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestScaleDownDeletesStalePDB covers D4's PDB cleanup hook: a live app
// scaled from 3 replicas (>=2, PDB applied) down to 1 (<2) must have its
// stale PodDisruptionBudget deleted, since sync only upserts and never
// deletes what a shrinking floor makes stale.
func TestScaleDownDeletesStalePDB(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	// Mark the app live directly (mirrors TestScaleLiveAppWithoutKube503's
	// pattern) so scale's sync path actually runs against the fake kube.
	id := appID(t, st, "web", "api")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":3}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale up: %d", resp.StatusCode)
	}
	resp.Body.Close()
	found := false
	for _, a := range *actions {
		if a == "patch poddisruptionbudgets" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want PDB applied at replicas=3, actions: %v", *actions)
	}

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/scale", admin, `{"replicas":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale down: %d", resp.StatusCode)
	}
	resp.Body.Close()
	found = false
	for _, a := range *actions {
		if a == "delete poddisruptionbudgets" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want stale PDB deleted at replicas=1, actions: %v", *actions)
	}
}

// TestProjectQuotaEndpointAppliesResourceQuotaAndLimitRange covers D4's
// happy path: setting a non-zero CPU/memory quota syncs both a
// ResourceQuota and a LimitRange into the project namespace.
func TestProjectQuotaEndpointAppliesResourceQuotaAndLimitRange(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/quota", admin, `{"cpu_milli":4000,"memory_mb":8192}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set quota: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"cpu_milli":4000`) || !strings.Contains(string(body), `"memory_mb":8192`) {
		t.Fatalf("body = %s", body)
	}

	found := map[string]bool{}
	for _, a := range *actions {
		found[a] = true
	}
	if !found["patch resourcequotas"] {
		t.Fatalf("want ResourceQuota applied, actions: %v", *actions)
	}
	if !found["patch limitranges"] {
		t.Fatalf("want LimitRange applied, actions: %v", *actions)
	}
}

// TestProjectQuotaEndpointZeroDeletesBoth covers the unlimited path: setting
// both cpu_milli and memory_mb back to 0 deletes the ResourceQuota and
// LimitRange (best-effort, never fails the request).
func TestProjectQuotaEndpointZeroDeletesBoth(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/quota", admin, `{"cpu_milli":4000,"memory_mb":8192}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/quota", admin, `{"cpu_milli":0,"memory_mb":0}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear quota: %d %s", resp.StatusCode, body)
	}

	found := map[string]bool{}
	for _, a := range *actions {
		found[a] = true
	}
	if !found["delete resourcequotas"] {
		t.Fatalf("want ResourceQuota deleted, actions: %v", *actions)
	}
	if !found["delete limitranges"] {
		t.Fatalf("want LimitRange deleted, actions: %v", *actions)
	}
}

// TestProjectQuotaEndpointNegativeRejected mirrors TestGPUQuotaEndpoint's
// negative-value guard.
func TestProjectQuotaEndpointNegativeRejected(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/quota", admin, `{"cpu_milli":-1,"memory_mb":0}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestProjectQuotaEndpointNonAdminForbidden checks the route is admin-only,
// mirroring the gpu-quota endpoint's access control.
func TestProjectQuotaEndpointNonAdminForbidden(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()
	member := seedUserToken(t, st, "m@b.co", "member")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/quota", member, `{"cpu_milli":1000,"memory_mb":0}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestGPUQuotaAppliesResourceQuotaAgainstFakeKube guards the D4 bugfix: the
// GPU quota feature's ResourceQuota apply (gpu.QuotaObject via setGPUQuota)
// had no GVR entry in kube.Apply's map and failed every time kube was
// actually configured — this exercises that path against a fake dynamic
// client and asserts the apply succeeds and is recorded.
func TestGPUQuotaAppliesResourceQuotaAgainstFakeKube(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"p1"}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/p1/gpu-quota", admin, `{"quota":2}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set gpu quota: %d %s (this is the ResourceQuota GVR bug if it 502s)", resp.StatusCode, body)
	}

	found := false
	for _, a := range *actions {
		if a == "patch resourcequotas" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want ResourceQuota applied, actions: %v", *actions)
	}
}
