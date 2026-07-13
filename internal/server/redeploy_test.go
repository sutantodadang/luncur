package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// rawStamp pulls the pod-template deploy annotation out of an app's rendered
// YAML.
func rawStamp(t *testing.T, srv *httptestServer, admin, app string) string {
	t.Helper()
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/"+app+"/raw", admin, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "luncur.dev/deploy:") {
			return strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	t.Fatalf("no deploy stamp in rendered manifest:\n%s", body)
	return ""
}

// TestRedeployImageApp covers the restart contract: an env edit re-applies the
// Secret but does NOT change the pod-template stamp (pods keep running until
// the user redeploys), while redeploy mints a new deployment id — changing the
// stamp — and bounces the pods even with an unchanged image and env.
func TestRedeployImageApp(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps/api/deploy", admin, `{"image":"nginx:1"}`); resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d", resp.StatusCode)
	}
	h1 := rawStamp(t, srv, admin, "api")

	// Env change must NOT alter the stamp — pods keep running until redeploy.
	if resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/env", admin, `{"key":"FOO","value":"bar"}`); resp.StatusCode != 204 {
		t.Fatalf("set env: want 204, got %d", resp.StatusCode)
	}
	h2 := rawStamp(t, srv, admin, "api")
	if h1 != h2 {
		t.Fatal("deploy stamp changed after env edit — pods would restart, but should only restart on redeploy")
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
	if h3 := rawStamp(t, srv, admin, "api"); h3 == h2 {
		t.Fatal("deploy stamp unchanged after redeploy — pods would not restart")
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
