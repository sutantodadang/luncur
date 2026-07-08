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

// sweepFake is a fake luncur server exposing just the sweep endpoints,
// capturing the decoded request body so tests can assert on the exact JSON
// shape the client sends.
type sweepFake struct {
	srv        *httptest.Server
	startBody  map[string]any
	startPath  string
	stopPath   string
	getPath    string
	sweepsResp []map[string]any
	getResp    map[string]any
}

func newSweepFake(t *testing.T) *sweepFake {
	t.Helper()
	f := &sweepFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/sweeps", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.startBody = body
		f.startPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "sw1", "status": "running", "metric": body["metric"],
			"direction": body["direction"], "parallel": body["parallel"],
			"trials": []map[string]any{
				{"id": "tr1", "state": "pending", "params": map[string]string{"lr": "0.1"}},
				{"id": "tr2", "state": "pending", "params": map[string]string{"lr": "0.2"}},
			},
		})
	})
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/sweeps", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f.sweepsResp)
	})
	mux.HandleFunc("GET /v1/projects/{project}/apps/{app}/sweeps/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.getPath = r.URL.Path
		json.NewEncoder(w).Encode(f.getResp)
	})
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/sweeps/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.stopPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "status": "stopped"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// TestSweepStartSendsParamsYAMLAndReportsTrials covers `luncur sweep start`:
// the CLI reads --params from disk, posts the raw YAML plus flags, and
// reports the trial count and parallelism from the response.
func TestSweepStartSendsParamsYAMLAndReportsTrials(t *testing.T) {
	f := newSweepFake(t)
	setCLIConfig(t, f.srv.URL)

	paramsPath := filepath.Join(t.TempDir(), "params.yaml")
	yamlContents := "lr: {min: 0.0001, max: 0.1, log: true}\nbatch_size: [16, 32]\n"
	if err := os.WriteFile(paramsPath, []byte(yamlContents), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "sweep", "start", "train", "--project", "ml",
		"--params", paramsPath, "--metric", "val_loss", "--max-trials", "12", "--parallel", "3", "--early-stop")
	if err != nil {
		t.Fatalf("sweep start: %v (%s)", err, out)
	}

	if f.startPath != "/v1/projects/ml/apps/train/sweeps" {
		t.Fatalf("start path = %q", f.startPath)
	}
	if f.startBody["params_yaml"] != yamlContents {
		t.Fatalf("params_yaml body = %v, want raw file contents", f.startBody["params_yaml"])
	}
	if f.startBody["metric"] != "val_loss" || f.startBody["direction"] != "min" {
		t.Fatalf("metric/direction = %v/%v", f.startBody["metric"], f.startBody["direction"])
	}
	if f.startBody["max_trials"] != float64(12) || f.startBody["parallel"] != float64(3) {
		t.Fatalf("max_trials/parallel = %v/%v", f.startBody["max_trials"], f.startBody["parallel"])
	}
	if f.startBody["early_stop"] != true {
		t.Fatalf("early_stop = %v, want true", f.startBody["early_stop"])
	}

	if !strings.Contains(out, "sweep sw1 started: 2 trials (parallel 3)") {
		t.Fatalf("want start confirmation, got %q", out)
	}
}

// TestSweepLsShowsCountsAndBest covers `luncur sweep ls`: the CLI must
// render one row per sweep with ID/STATUS/METRIC/DONE-TOTAL/BEST.
func TestSweepLsShowsCountsAndBest(t *testing.T) {
	f := newSweepFake(t)
	setCLIConfig(t, f.srv.URL)

	best := 0.42
	f.sweepsResp = []map[string]any{
		{"id": "sw1", "status": "done", "metric": "val_loss",
			"counts": map[string]int{"done": 3, "pruned": 1}, "best_value": best},
		{"id": "sw2", "status": "running", "metric": "acc",
			"counts": map[string]int{"pending": 2, "running": 1}},
	}

	out, err := run(t, "sweep", "ls", "train", "--project", "ml")
	if err != nil {
		t.Fatalf("sweep ls: %v (%s)", err, out)
	}
	for _, want := range []string{"sw1", "done", "val_loss", "4", "0.42", "sw2", "running", "acc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in sweep ls output, got %q", want, out)
		}
	}
}

// TestSweepStatusShowsTrialsAndBest covers `luncur sweep status <id>`: the
// CLI must render a trials table (with compact k=v params) plus a best line.
func TestSweepStatusShowsTrialsAndBest(t *testing.T) {
	f := newSweepFake(t)
	setCLIConfig(t, f.srv.URL)

	val := 0.1
	f.getResp = map[string]any{
		"id": "sw1", "status": "running", "metric": "val_loss", "direction": "min",
		"best_trial_id": "tr2", "best_value": val,
		"trials": []map[string]any{
			{"id": "tr1", "state": "done", "params": map[string]string{"lr": "0.2", "bs": "16"}, "metric_value": 0.5},
			{"id": "tr2", "state": "done", "params": map[string]string{"lr": "0.1", "bs": "32"}, "metric_value": 0.1},
		},
	}

	out, err := run(t, "sweep", "status", "sw1", "--app", "train", "--project", "ml")
	if err != nil {
		t.Fatalf("sweep status: %v (%s)", err, out)
	}
	if f.getPath != "/v1/projects/ml/apps/train/sweeps/sw1" {
		t.Fatalf("get path = %q", f.getPath)
	}
	for _, want := range []string{"tr1", "tr2", "bs=16 lr=0.2", "bs=32 lr=0.1", "best: trial tr2", "val_loss=0.1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in sweep status output, got %q", want, out)
		}
	}
}

// TestSweepStopReportsStatus covers `luncur sweep stop <id>`.
func TestSweepStopReportsStatus(t *testing.T) {
	f := newSweepFake(t)
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "sweep", "stop", "sw1", "--app", "train", "--project", "ml")
	if err != nil {
		t.Fatalf("sweep stop: %v (%s)", err, out)
	}
	if f.stopPath != "/v1/projects/ml/apps/train/sweeps/sw1/stop" {
		t.Fatalf("stop path = %q", f.stopPath)
	}
	if !strings.Contains(out, "sweep sw1: stopped") {
		t.Fatalf("want stop confirmation, got %q", out)
	}
}
