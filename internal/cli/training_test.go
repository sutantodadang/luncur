package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// trainingFake is a minimal fake luncur server exposing just the runs and
// training-defaults endpoints, capturing the decoded request bodies so
// tests can assert on the exact nodes/framework the CLI sends.
type trainingFake struct {
	srv         *httptest.Server
	runBody     map[string]any
	trainingReq string // captured raw path, so the test can assert project/app
	trainBody   map[string]any
}

func newTrainingFake(t *testing.T) *trainingFake {
	t.Helper()
	f := &trainingFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/runs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.runBody = body
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "status": "running", "job": "train-run-1",
			"nodes": body["nodes"], "framework": body["framework"],
			"started_at": "2026-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("PUT /v1/projects/{project}/apps/{app}/training", func(w http.ResponseWriter, r *http.Request) {
		f.trainingReq = r.URL.Path
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.trainBody = body
		json.NewEncoder(w).Encode(map[string]any{
			"app": r.PathValue("app"), "nodes": body["nodes"], "framework": body["framework"],
		})
	})
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "status": "succeeded", "job": "train-run-1", "nodes": 3, "started_at": "2026-01-01T00:00:00Z"},
			{"id": 2, "status": "succeeded", "job": "train-run-2", "nodes": 1, "started_at": "2026-01-02T00:00:00Z"},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// TestRunNodesFrameworkFlags covers run --nodes/--framework: the CLI must
// forward both to POST .../runs and report the run as started.
func TestRunNodesFrameworkFlags(t *testing.T) {
	f := newTrainingFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "run", "train", "--project", "ml", "--nodes", "3", "--framework", "torchrun", "--detach")
	if err != nil {
		t.Fatalf("run: %v (%s)", err, out)
	}
	if f.runBody["nodes"] != float64(3) || f.runBody["framework"] != "torchrun" {
		t.Fatalf("run body = %v", f.runBody)
	}
	if !strings.Contains(out, "run 1 started") {
		t.Fatalf("want 'run 1 started', got %q", out)
	}
}

// TestRunDefaultsOmitNodesFramework covers the zero-value case: without
// --nodes/--framework the CLI must not force nodes=0 or framework="" onto
// the wire distinctly from "unset" — CreateRun omits both from the body.
func TestRunDefaultsOmitNodesFramework(t *testing.T) {
	f := newTrainingFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "run", "train", "--project", "ml", "--detach")
	if err != nil {
		t.Fatalf("run: %v (%s)", err, out)
	}
	if _, ok := f.runBody["nodes"]; ok {
		t.Fatalf("nodes should be omitted when unset, got %v", f.runBody)
	}
	if _, ok := f.runBody["framework"]; ok {
		t.Fatalf("framework should be omitted when unset, got %v", f.runBody)
	}
}

// TestAppTrainingCommand covers `luncur app training`: the CLI must PUT
// nodes/framework to the training-defaults endpoint.
func TestAppTrainingCommand(t *testing.T) {
	f := newTrainingFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "app", "training", "train", "--project", "ml", "--nodes", "4", "--framework", "torch")
	if err != nil {
		t.Fatalf("app training: %v (%s)", err, out)
	}
	if f.trainBody["nodes"] != float64(4) || f.trainBody["framework"] != "torch" {
		t.Fatalf("training body = %v", f.trainBody)
	}
	if f.trainingReq != "/v1/projects/ml/apps/train/training" {
		t.Fatalf("training path = %q", f.trainingReq)
	}
	if !strings.Contains(out, "nodes=4") || !strings.Contains(out, "torch") {
		t.Fatalf("output = %q", out)
	}
}

// TestRunListNodesColumn covers run ls: when any run has nodes>1, every row
// gains a nodes=N field.
func TestRunListNodesColumn(t *testing.T) {
	f := newTrainingFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "run", "ls", "train", "--project", "ml")
	if err != nil {
		t.Fatalf("run ls: %v (%s)", err, out)
	}
	if !strings.Contains(out, "nodes=3") || !strings.Contains(out, "nodes=1") {
		t.Fatalf("want nodes column on every row, got %q", out)
	}
}
