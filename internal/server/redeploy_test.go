package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestPodConfigHash(t *testing.T) {
	base := podConfigHash(map[string]string{"A": "1", "B": "2"}, "dep1")

	// Order-independent.
	if podConfigHash(map[string]string{"B": "2", "A": "1"}, "dep1") != base {
		t.Fatal("hash should not depend on map order")
	}
	// A changed value changes the hash.
	if podConfigHash(map[string]string{"A": "1", "B": "3"}, "dep1") == base {
		t.Fatal("hash should change when an env value changes")
	}
	// A different deployment id changes the hash (redeploy bounces pods).
	if podConfigHash(map[string]string{"A": "1", "B": "2"}, "dep2") == base {
		t.Fatal("hash should change when the deployment id changes")
	}
	// No "AB"+"" vs "A"+"B" collisions across the key/value boundary.
	if podConfigHash(map[string]string{"A": "BC"}, "d") == podConfigHash(map[string]string{"AB": "C"}, "d") {
		t.Fatal("key/value boundary collision")
	}
}

// rawHash pulls the pod-template config-hash out of an app's rendered YAML.
func rawHash(t *testing.T, srv *httptestServer, admin, app string) string {
	t.Helper()
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/"+app+"/raw", admin, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "luncur.dev/config-hash:") {
			return strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	t.Fatalf("no config-hash in rendered manifest:\n%s", body)
	return ""
}

// TestRedeployImageApp covers the end-to-end restart mechanism: deploying an
// image stamps a config hash, changing env changes the hash (so the pods
// roll), and redeploy creates a new live deployment whose fresh id changes the
// hash again — bouncing pods even with an unchanged image and env.
func TestRedeployImageApp(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"nginx:1"}`); resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d", resp.StatusCode)
	}
	h1 := rawHash(t, srv, admin, "api")

	// Env change must alter the hash (env is a Secret EnvFrom; only a
	// pod-template change restarts the pods).
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/env", admin, `{"key":"FOO","value":"bar"}`); resp.StatusCode != 204 {
		t.Fatalf("set env: want 204, got %d", resp.StatusCode)
	}
	h2 := rawHash(t, srv, admin, "api")
	if h1 == h2 {
		t.Fatal("config hash unchanged after env change — pods would not restart")
	}

	// Redeploy: new deployment, same image, 200 live.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/redeploy", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("redeploy: want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["status"] != "live" {
		t.Fatalf("redeploy status: %v", out["status"])
	}

	d, err := st.LatestDeployment(appID(t, st, "web", "api"))
	if err != nil || d.ImageRef != "nginx:1" || d.Status != "live" {
		t.Fatalf("latest deployment after redeploy: %+v %v", d, err)
	}
	if d.Seq != 2 {
		t.Fatalf("redeploy should be deploy #2, got seq %d", d.Seq)
	}

	// A new deployment id rolls the pods even with unchanged image and env.
	if h3 := rawHash(t, srv, admin, "api"); h3 == h2 {
		t.Fatal("config hash unchanged after redeploy — pods would not restart")
	}
}

func TestRedeployNeverDeployed(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/redeploy", admin, ""); resp.StatusCode != 409 {
		t.Fatalf("redeploy never-deployed app: want 409, got %d", resp.StatusCode)
	}
}

// TestRedeployGitAppTakesBuildPath covers the git-source branch: redeploy
// routes through deployGitApp (a fresh build) rather than re-applying a
// prebuilt image. This test server has no data dir, so the build source is
// unavailable — a 503 build_unavailable proves the git branch was taken (the
// image branch would 409 no_deploy, since the app was never deployed).
func TestRedeployGitAppTakesBuildPath(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin,
		`{"name":"g","port":8080,"git_url":"https://github.com/me/pub"}`).Body.Close()

	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/g/redeploy", admin, "")
	if resp.StatusCode != 503 {
		t.Fatalf("redeploy git app without build source: want 503, got %d", resp.StatusCode)
	}
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Error.Code != "build_unavailable" {
		t.Fatalf("git redeploy error code: %q", out.Error.Code)
	}
}
