package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunsAPI(t *testing.T) {
	runWatchPoll = 10 * time.Millisecond
	t.Cleanup(func() { runWatchPoll = 5 * time.Second })

	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	// Runs on a non-job app -> kind_mismatch.
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/api/runs", admin, `{}`)
	if resp.StatusCode != 400 || errCode(t, mustReadBody(t, resp)) != "kind_mismatch" {
		t.Fatalf("non-job run: want 400 kind_mismatch, got %d", resp.StatusCode)
	}

	// Run before any deploy -> not_deployed.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{}`)
	if resp.StatusCode != 409 || errCode(t, mustReadBody(t, resp)) != "not_deployed" {
		t.Fatalf("undeployed run: want 409 not_deployed, got %d", resp.StatusCode)
	}

	// Deploy an image (job kind: applies Secret/PVCs only, marks live).
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/deploy", admin, `{"image":"trainer:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("deploy: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	// Trigger a run.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/train/runs", admin, `{}`)
	if resp.StatusCode != 202 {
		t.Fatalf("run: want 202, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var run map[string]any
	json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if run["job"] != "train-run-1" || run["status"] != "running" {
		t.Fatalf("run: %+v", run)
	}
	if !strings.Contains(strings.Join(*actions, ","), "patch jobs") {
		t.Fatalf("no Job applied: %v", *actions)
	}

	// History lists the run (its status may already be failed: the fake
	// dynamic client errors the watcher's Job poll).
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs", admin, "")
	var runs []map[string]any
	json.NewDecoder(resp.Body).Decode(&runs)
	resp.Body.Close()
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}

	// Single-run fetch.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs/1", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get run: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown run id.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs/99", admin, "")
	if resp.StatusCode != 404 {
		t.Fatalf("missing run: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Malformed run id.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/apps/train/runs/zzz", admin, "")
	if resp.StatusCode != 400 {
		t.Fatalf("bad run id: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
