package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSetHealthRoundTrip(t *testing.T) {
	srv, st := testServer(t) // no kube; app never live, so no sync required
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/health", admin, `{"path":"/healthz"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set health: want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["health_path"] != "/healthz" {
		t.Fatalf("response: %v", out)
	}

	a, err := st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil || a.HealthPath != "/healthz" {
		t.Fatalf("stored health path: %+v %v", a, err)
	}

	// GET app surfaces it too.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api", admin, "")
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["health_path"] != "/healthz" {
		t.Fatalf("get app health_path: %v", got)
	}

	// Clear.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/health", admin, `{"path":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear health: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	a, err = st.GetApp(mustProjectID(t, st, "web"), "api")
	if err != nil || a.HealthPath != "" {
		t.Fatalf("after clear: %+v %v", a, err)
	}
}

func TestSetHealthInvalid400(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	for _, path := range []string{"healthz", "/health z"} {
		resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/health", admin, `{"path":"`+path+`"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("path %q: want 400, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestSetHealthEjected409(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "").Body.Close()

	assertAppEjected(t, doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/health", admin, `{"path":"/healthz"}`), "set health on ejected app")
}

func TestSetHealthLiveAppSyncsManifestWithProbes(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"nginx:1"}`).Body.Close()

	before := len(*actions)
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/health", admin, `{"path":"/healthz"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set health: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if len(*actions) <= before {
		t.Fatal("setting health path on a live app should re-apply to cluster")
	}

	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/api/raw", admin, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw manifest: want 200, got %d", resp.StatusCode)
	}
	var buf strings.Builder
	io.Copy(&buf, resp.Body)
	resp.Body.Close()
	yamlStr := buf.String()
	if !strings.Contains(yamlStr, "readinessProbe") || !strings.Contains(yamlStr, "livenessProbe") {
		t.Fatalf("rendered manifest missing probes:\n%s", yamlStr)
	}
	if !strings.Contains(yamlStr, "/healthz") {
		t.Fatalf("rendered manifest missing health path:\n%s", yamlStr)
	}
}
