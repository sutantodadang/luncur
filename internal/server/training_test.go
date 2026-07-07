package server

import (
	"encoding/json"
	"testing"
)

// TestSetTrainingEndpoint covers the happy path: PUT persists nodes/framework
// on a kind=job app and echoes them back.
func TestSetTrainingEndpoint(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/apps/train/training", admin, `{"nodes":4,"framework":"torchrun"}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("set training: want 200, got %d (%s)", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["app"] != "train" || out["nodes"] != float64(4) || out["framework"] != "torchrun" {
		t.Fatalf("response = %+v", out)
	}

	got, err := st.GetApp(mustProjectID(t, st, "ml"), "train")
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes != 4 || got.Framework != "torchrun" {
		t.Fatalf("app row: nodes=%d framework=%q, want 4 torchrun", got.Nodes, got.Framework)
	}
}

// TestSetTrainingKindMismatch covers requireJobApp's guard: a non-job app
// gets 400 kind_mismatch, not a store update.
func TestSetTrainingKindMismatch(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/apps/api/training", admin, `{"nodes":2}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 || errCode(t, body) != "kind_mismatch" {
		t.Fatalf("kind mismatch: want 400 kind_mismatch, got %d (%s)", resp.StatusCode, body)
	}
}

// TestSetTrainingOverBudget covers the GPU budget delta check: a gpu=1
// nodes=1 app raising to nodes=3 needs 2 more GPUs (1×3 − 1×1); a quota of 2
// with the app's own footprint (1) already counted leaves only 1 free.
func TestSetTrainingOverBudget(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/gpu-quota", admin, `{"quota":2}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job","gpu":1}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/apps/train/training", admin, `{"nodes":3}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 || errCode(t, body) != "over_budget" {
		t.Fatalf("over budget: want 400 over_budget, got %d (%s)", resp.StatusCode, body)
	}

	got, err := st.GetApp(mustProjectID(t, st, "ml"), "train")
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes != 1 {
		t.Fatalf("over-budget request must not persist: nodes=%d", got.Nodes)
	}
}

// TestSetTrainingBadFramework covers store.SetAppTraining's framework
// validation surfacing as 400 bad_request through the handler.
func TestSetTrainingBadFramework(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin, `{"name":"train","kind":"job"}`).Body.Close()

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/apps/train/training", admin, `{"nodes":2,"framework":"mpi"}`)
	body := mustReadBody(t, resp)
	if resp.StatusCode != 400 || errCode(t, body) != "bad_request" {
		t.Fatalf("bad framework: want 400 bad_request, got %d (%s)", resp.StatusCode, body)
	}
}
