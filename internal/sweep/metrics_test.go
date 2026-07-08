package sweep

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseMetricLinesLastMatchWins(t *testing.T) {
	log := strings.Join([]string{
		"some noise",
		"luncur-metric: val_loss=0.9 step=1",
		"luncur-metric: val_loss=0.5 step=2",
		"luncur-metric: acc=0.99 step=2", // other metric, ignored
	}, "\n")
	obs := ParseMetricLines(strings.NewReader(log), "val_loss")
	if !obs.Found || obs.Value != 0.5 || obs.Step != 2 {
		t.Fatalf("obs = %+v, want last val_loss (0.5, step 2)", obs)
	}
}

func TestParseMetricLinesOtherNamesIgnored(t *testing.T) {
	obs := ParseMetricLines(strings.NewReader("luncur-metric: acc=0.9 step=1\n"), "val_loss")
	if obs.Found {
		t.Fatalf("obs = %+v, want not found", obs)
	}
}

func TestParseMetricLinesMalformedSkipped(t *testing.T) {
	log := strings.Join([]string{
		"luncur-metric: val_loss=notanumber",
		"luncur-metric: val_loss=0.3 step=5",
	}, "\n")
	obs := ParseMetricLines(strings.NewReader(log), "val_loss")
	if !obs.Found || obs.Value != 0.3 || obs.Step != 5 {
		t.Fatalf("obs = %+v, want (0.3, step 5)", obs)
	}
}

func TestParseMetricLinesStepOptional(t *testing.T) {
	obs := ParseMetricLines(strings.NewReader("luncur-metric: val_loss=0.7\n"), "val_loss")
	if !obs.Found || obs.Value != 0.7 || obs.Step != 0 {
		t.Fatalf("obs = %+v, want (0.7, step 0)", obs)
	}
}

func TestParseMetricLinesPodPrefixed(t *testing.T) {
	obs := ParseMetricLines(strings.NewReader("[trial-abc123-0] luncur-metric: val_loss=0.2 step=9\n"), "val_loss")
	if !obs.Found || obs.Value != 0.2 || obs.Step != 9 {
		t.Fatalf("obs = %+v, want (0.2, step 9)", obs)
	}
}

func TestParseMetricLinesNoMatch(t *testing.T) {
	obs := ParseMetricLines(strings.NewReader("just some log output\nnothing here\n"), "val_loss")
	if obs.Found {
		t.Fatalf("obs = %+v, want not found", obs)
	}
}

func canncedMLflowResponse(runName string) []byte {
	resp := map[string]any{
		"runs": []map[string]any{
			{
				"data": map[string]any{
					"metrics": []map[string]any{
						{"key": "val_loss", "value": 0.42, "timestamp": 1000, "step": 7},
						{"key": "acc", "value": 0.9, "timestamp": 1000, "step": 7},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestMLflowLatest(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = buf
		w.Header().Set("Content-Type", "application/json")
		w.Write(canncedMLflowResponse("trial-abc"))
	}))
	defer srv.Close()

	m := &MLflow{BaseURL: srv.URL}
	obs, err := m.Latest(context.Background(), "trial-abc", "val_loss")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Found || obs.Value != 0.42 || obs.Step != 7 {
		t.Fatalf("obs = %+v, want (0.42, step 7)", obs)
	}
	if !strings.Contains(string(gotBody), "trial-abc") {
		t.Fatalf("request body must contain run name, got %s", gotBody)
	}
}

func TestMLflowLatestMissingRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"runs":[]}`))
	}))
	defer srv.Close()

	m := &MLflow{BaseURL: srv.URL}
	obs, err := m.Latest(context.Background(), "trial-missing", "val_loss")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Found {
		t.Fatalf("obs = %+v, want not found", obs)
	}
}

func TestMLflowLatestMissingMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(canncedMLflowResponse("trial-abc"))
	}))
	defer srv.Close()

	m := &MLflow{BaseURL: srv.URL}
	obs, err := m.Latest(context.Background(), "trial-abc", "not_a_real_metric")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Found {
		t.Fatalf("obs = %+v, want not found", obs)
	}
}

func TestMLflowLatestServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom: internal error"))
	}))
	defer srv.Close()

	m := &MLflow{BaseURL: srv.URL}
	_, err := m.Latest(context.Background(), "trial-abc", "val_loss")
	if err == nil {
		t.Fatal("want error on 500")
	}
}
