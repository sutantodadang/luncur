package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pipelineFake is a fake luncur server exposing just the pipeline endpoints,
// capturing the decoded request body/path so tests can assert on the exact
// wire shape the client sends (sweepFake's pattern).
type pipelineFake struct {
	srv *httptest.Server

	createBody map[string]any
	createPath string
	createResp map[string]any

	updateBody map[string]any
	updatePath string
	updateResp map[string]any

	lsResp []map[string]any

	runPath string
	runResp map[string]any

	statusPath string
	statusResp map[string]any

	stopPath string
	stopResp map[string]any

	rmPath string
}

func newPipelineFake(t *testing.T) *pipelineFake {
	t.Helper()
	f := &pipelineFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{project}/pipelines", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.createBody = body
		f.createPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(f.createResp)
	})
	mux.HandleFunc("PUT /v1/projects/{project}/pipelines/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.updateBody = body
		f.updatePath = r.URL.Path
		json.NewEncoder(w).Encode(f.updateResp)
	})
	mux.HandleFunc("GET /v1/projects/{project}/pipelines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f.lsResp)
	})
	mux.HandleFunc("POST /v1/projects/{project}/pipelines/{name}/runs", func(w http.ResponseWriter, r *http.Request) {
		f.runPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(f.runResp)
	})
	mux.HandleFunc("GET /v1/projects/{project}/pipelines/{name}/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.statusPath = r.URL.Path
		json.NewEncoder(w).Encode(f.statusResp)
	})
	mux.HandleFunc("POST /v1/projects/{project}/pipelines/{name}/runs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.stopPath = r.URL.Path
		json.NewEncoder(w).Encode(f.stopResp)
	})
	mux.HandleFunc("DELETE /v1/projects/{project}/pipelines/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.rmPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// TestPipelineCreatePostsRawYAML covers `luncur pipeline create`: the CLI
// reads --file from disk and posts its raw contents plus name/engine.
func TestPipelineCreatePostsRawYAML(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)
	f.createResp = map[string]any{"id": "pl1", "name": "pipe", "engine": "native"}

	yamlPath := filepath.Join(t.TempDir(), "pipeline.yaml")
	yamlContents := "steps:\n  train:\n    app: train\n"
	if err := os.WriteFile(yamlPath, []byte(yamlContents), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "pipeline", "create", "pipe", "--project", "ml", "--file", yamlPath, "--engine", "native")
	if err != nil {
		t.Fatalf("pipeline create: %v (%s)", err, out)
	}

	if f.createPath != "/v1/projects/ml/pipelines" {
		t.Fatalf("create path = %q", f.createPath)
	}
	if f.createBody["name"] != "pipe" || f.createBody["yaml"] != yamlContents || f.createBody["engine"] != "native" {
		t.Fatalf("create body = %+v", f.createBody)
	}
	if !strings.Contains(out, "pipeline pipe created") {
		t.Fatalf("want create confirmation, got %q", out)
	}
}

// TestPipelineCreateHelpMentionsPlaintextEnv covers the required help-text
// warning: step env in pipeline.yaml is stored in plaintext.
func TestPipelineCreateHelpMentionsPlaintextEnv(t *testing.T) {
	out, err := run(t, "pipeline", "create", "--help")
	if err != nil {
		t.Fatalf("pipeline create --help: %v (%s)", err, out)
	}
	if !strings.Contains(out, "plaintext") {
		t.Fatalf("create --help must mention plaintext env, got %q", out)
	}
}

// TestPipelineUpdateOmitsUnsetFields covers `luncur pipeline update`: only
// flags actually passed are sent, so an omitted --file/--engine keeps the
// server-side value unchanged.
func TestPipelineUpdateOmitsUnsetFields(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)
	f.updateResp = map[string]any{"id": "pl1", "name": "pipe", "engine": "argo"}

	out, err := run(t, "pipeline", "update", "pipe", "--project", "ml", "--engine", "argo")
	if err != nil {
		t.Fatalf("pipeline update: %v (%s)", err, out)
	}
	if f.updatePath != "/v1/projects/ml/pipelines/pipe" {
		t.Fatalf("update path = %q", f.updatePath)
	}
	if _, hasYAML := f.updateBody["yaml"]; hasYAML {
		t.Fatalf("update body must omit yaml when --file wasn't passed: %+v", f.updateBody)
	}
	if f.updateBody["engine"] != "argo" {
		t.Fatalf("update body engine = %v, want argo", f.updateBody["engine"])
	}
	if !strings.Contains(out, "pipeline pipe updated") {
		t.Fatalf("want update confirmation, got %q", out)
	}
}

// TestPipelineLsShowsEngineAndLastRun covers `luncur pipeline ls`: one row
// per pipeline with NAME/ENGINE/LAST RUN/STATUS, "-" for a pipeline that has
// never run, and "native" for a pipeline with no engine pinned.
func TestPipelineLsShowsEngineAndLastRun(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)
	f.lsResp = []map[string]any{
		{"id": "pl1", "name": "train-pipe", "engine": "", "last_run": map[string]any{"id": "run1", "status": "done", "started_at": "2026-01-01 00:00:00"}},
		{"id": "pl2", "name": "empty-pipe", "engine": "argo", "last_run": nil},
	}

	out, err := run(t, "pipeline", "ls", "--project", "ml")
	if err != nil {
		t.Fatalf("pipeline ls: %v (%s)", err, out)
	}
	for _, want := range []string{"train-pipe", "native", "run1", "done", "empty-pipe", "argo", "-"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in pipeline ls output, got %q", want, out)
		}
	}
}

// TestPipelineRunReportsStepCount covers `luncur pipeline run`.
func TestPipelineRunReportsStepCount(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)
	f.runResp = map[string]any{
		"id": "run1", "status": "running",
		"steps": []map[string]any{
			{"name": "a", "kind": "app", "state": "running", "attempt": 1},
			{"name": "b", "kind": "app", "state": "pending", "attempt": 0},
		},
	}

	out, err := run(t, "pipeline", "run", "pipe", "--project", "ml")
	if err != nil {
		t.Fatalf("pipeline run: %v (%s)", err, out)
	}
	if f.runPath != "/v1/projects/ml/pipelines/pipe/runs" {
		t.Fatalf("run path = %q", f.runPath)
	}
	if !strings.Contains(out, "run run1 started: 2 steps") {
		t.Fatalf("want run confirmation, got %q", out)
	}
}

// TestPipelineStatusShowsStepsAndWarning covers `luncur pipeline status`:
// the CLI must render a steps table plus a run-status footer and, when set,
// a warning line.
func TestPipelineStatusShowsStepsAndWarning(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)
	f.statusResp = map[string]any{
		"id": "run1", "status": "failed", "warning": "mlflow unreachable",
		"steps": []map[string]any{
			{"name": "a", "kind": "app", "state": "done", "attempt": 1, "detail": "exit 0",
				"started_at": "2026-01-01 00:00:00", "finished_at": "2026-01-01 00:00:05"},
			{"name": "b", "kind": "app", "state": "failed", "attempt": 1, "detail": "exit 1"},
		},
	}

	out, err := run(t, "pipeline", "status", "run1", "--pipeline", "pipe", "--project", "ml")
	if err != nil {
		t.Fatalf("pipeline status: %v (%s)", err, out)
	}
	if f.statusPath != "/v1/projects/ml/pipelines/pipe/runs/run1" {
		t.Fatalf("status path = %q", f.statusPath)
	}
	for _, want := range []string{"a", "app", "done", "exit 0", "5s", "b", "failed", "exit 1", "run run1: failed", "warning: mlflow unreachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in pipeline status output, got %q", want, out)
		}
	}
}

// TestPipelineStopReportsStatus covers `luncur pipeline stop`.
func TestPipelineStopReportsStatus(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)
	f.stopResp = map[string]any{"id": "run1", "status": "stopped"}

	out, err := run(t, "pipeline", "stop", "run1", "--pipeline", "pipe", "--project", "ml")
	if err != nil {
		t.Fatalf("pipeline stop: %v (%s)", err, out)
	}
	if f.stopPath != "/v1/projects/ml/pipelines/pipe/runs/run1/stop" {
		t.Fatalf("stop path = %q", f.stopPath)
	}
	if !strings.Contains(out, "run run1: stopped") {
		t.Fatalf("want stop confirmation, got %q", out)
	}
}

// TestPipelineRmDeletes covers `luncur pipeline rm`.
func TestPipelineRmDeletes(t *testing.T) {
	f := newPipelineFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "pipeline", "rm", "pipe", "--project", "ml")
	if err != nil {
		t.Fatalf("pipeline rm: %v (%s)", err, out)
	}
	if f.rmPath != "/v1/projects/ml/pipelines/pipe" {
		t.Fatalf("rm path = %q", f.rmPath)
	}
	if !strings.Contains(out, "pipeline pipe deleted") {
		t.Fatalf("want rm confirmation, got %q", out)
	}
}
