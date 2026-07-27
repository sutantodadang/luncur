package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDeployLog writes a fake build log for deployID directly at the path
// build.Source.LogPath would compute (dataDir/logs/<id>.log) — cheaper than
// pulling in the build package just to get the same path formula.
func writeDeployLog(t *testing.T, dataDir, deployID, content string) {
	t.Helper()
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, deployID+".log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUIOverviewErrorCardOnFailedDeploy checks Overview's 3-line error
// contract (DESIGN.md "Error contract") renders for a failed latest
// deploy: what broke (deploy #, stage), why (deployFailureHint against the
// build log tail), and the next command plus a link into Ship's deploy
// history row for that deploy's log.
func TestUIOverviewErrorCardOnFailedDeploy(t *testing.T) {
	dataDir := t.TempDir()
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, DataDir: dataDir})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	// ImageRef empty -> the deploy died in "building" (deployFailedStage).
	d, err := st.CreateDeployment(appID(t, st, "proj", "web"), "failed", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	writeDeployLog(t, dataDir, d.ID, "Step 4/10 : RUN npm install\nnpm ERR! code E404\nnpm ERR! registry fetch failed")

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if !strings.Contains(body, "class=\"err3") {
		t.Fatalf("app page missing error card, got:\n%s", body)
	}
	if !strings.Contains(body, "Deploy #"+fmt.Sprintf("%d", d.Seq)+" failed while building.") {
		t.Fatalf("app page missing what-broke line, got:\n%s", body)
	}
	if !strings.Contains(body, `dependency install or compile step failed: &#34;npm ERR! code E404&#34;`) &&
		!strings.Contains(body, `dependency install or compile step failed: "npm ERR! code E404"`) {
		t.Fatalf("app page missing why line, got:\n%s", body)
	}
	if !strings.Contains(body, "luncur logs web --deploy "+fmt.Sprintf("%d", d.Seq)+" --project proj") {
		t.Fatalf("app page missing next-command CLI echo, got:\n%s", body)
	}
	if !strings.Contains(body, "?tab=ship#deploy-"+fmt.Sprintf("%d", d.Seq)) {
		t.Fatalf("app page missing view-log link into Ship's deploy row, got:\n%s", body)
	}
}

// TestUIOverviewErrorCardAbsentOnLive checks the error card never renders
// when the latest deploy is live.
func TestUIOverviewErrorCardAbsentOnLive(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	if _, err := st.CreateDeployment(appID(t, st, "proj", "web"), "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if strings.Contains(body, "class=\"err3") || strings.Contains(body, "failed while") {
		t.Fatalf("app page should not show the error card for a live deploy, got:\n%s", body)
	}
}

// TestUILaunchSequenceNeverDeployed checks Overview replaces the status
// board + pipeline with the Launch Sequence checklist for an app that has
// never deployed (DESIGN.md "Launch Sequence"): a git-connected app with no
// env vars and no deploys lands on step 2 ("Set environment variables")
// current, with the env form expanded inline.
func TestUILaunchSequenceNeverDeployed(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080,"git_url":"https://example.com/repo.git"}`).Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if !strings.Contains(body, "LAUNCH SEQUENCE") {
		t.Fatalf("app page missing Launch Sequence checklist, got:\n%s", body)
	}
	if strings.Contains(body, "<h2>Status</h2>") || strings.Contains(body, "<h2>Deploy pipeline</h2>") {
		t.Fatalf("Launch Sequence should replace the status board + pipeline, got:\n%s", body)
	}
	if !strings.Contains(body, "Connect repository") {
		t.Fatalf("app page missing step 1 label, got:\n%s", body)
	}
	if !strings.Contains(body, "Set environment variables") {
		t.Fatalf("app page missing step 2 label, got:\n%s", body)
	}
	// Step 2 is current: its env form (shared with the Wire tab) expands
	// inline, plus that form's own CLI-echo.
	if !strings.Contains(body, `name="key" placeholder="KEY"`) {
		t.Fatalf("app page missing inline env form for the current step, got:\n%s", body)
	}
	if !strings.Contains(body, "luncur env set web KEY=VALUE") {
		t.Fatalf("app page missing env form's CLI echo, got:\n%s", body)
	}
	if !strings.Contains(body, "First deploy") || !strings.Contains(body, "Attach domain (optional)") {
		t.Fatalf("app page missing remaining checklist steps, got:\n%s", body)
	}
}

// TestUILaunchSequenceAbsentOnceDeployed checks the checklist disappears
// for good the moment the app has any deployment row at all, replaced by
// the normal status board + pipeline.
func TestUILaunchSequenceAbsentOnceDeployed(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080,"git_url":"https://example.com/repo.git"}`).Body.Close()
	if _, err := st.CreateDeployment(appID(t, st, "proj", "web"), "building", "", 0); err != nil {
		t.Fatal(err)
	}

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ck := uiSessionCookie(t, st, u.ID)
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/projects/proj/apps/web", ck)
	if status != http.StatusOK {
		t.Fatalf("GET app page: want 200, got %d", status)
	}
	if strings.Contains(body, "LAUNCH SEQUENCE") {
		t.Fatalf("Launch Sequence should be gone once a deployment exists, got:\n%s", body)
	}
	if !strings.Contains(body, "<h2>Status</h2>") || !strings.Contains(body, "<h2>Deploy pipeline</h2>") {
		t.Fatalf("app page missing status board + pipeline, got:\n%s", body)
	}
}
