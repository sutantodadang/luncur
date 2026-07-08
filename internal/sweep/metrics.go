package sweep

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Obs is one metric observation for a trial.
type Obs struct {
	Value float64
	Step  int64
	Found bool
}

// metricLineRe matches the log-line metric contract:
//
//	luncur-metric: <name>=<value> [step=<n>]
//
// An optional leading `[<pod>] ` prefix is also accepted — the run-log
// harvester feeds SSE-style pod-prefixed lines (see internal/server/runs.go).
var metricLineRe = regexp.MustCompile(`^(?:\[[^\]]*\]\s+)?luncur-metric:\s+(\w[\w.\-/]*)=(-?[0-9.eE+\-]+)(?:\s+step=(\d+))?\s*$`)

// ParseMetricLines scans log output for the LAST line matching the
// luncur-metric contract for the wanted metric name. Malformed lines
// (unparseable value/step) are skipped, never errors; Found stays false if
// no matching line is ever seen.
func ParseMetricLines(r io.Reader, metric string) Obs {
	var last Obs
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		m := metricLineRe.FindStringSubmatch(sc.Text())
		if m == nil || m[1] != metric {
			continue
		}
		val, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		var step int64
		if m[3] != "" {
			step, err = strconv.ParseInt(m[3], 10, 64)
			if err != nil {
				continue
			}
		}
		last = Obs{Value: val, Step: step, Found: true}
	}
	return last
}

// MLflow reads latest metric values from an MLflow tracking server. BaseURL
// is the addon's in-cluster URL; HTTP defaults to a 10s-timeout client when
// nil.
type MLflow struct {
	BaseURL string
	HTTP    *http.Client
}

func (m *MLflow) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

type mlflowSearchResponse struct {
	Runs []struct {
		Data struct {
			Metrics []struct {
				Key       string  `json:"key"`
				Value     float64 `json:"value"`
				Timestamp int64   `json:"timestamp"`
				Step      int64   `json:"step"`
			} `json:"metrics"`
		} `json:"data"`
	} `json:"runs"`
}

// Latest returns the most recent value of metric for the trial's run.
// Found is false when the run or the metric is absent (not an error).
func (m *MLflow) Latest(ctx context.Context, runName, metric string) (Obs, error) {
	// Trial names are luncur-generated ("trial-<nanoid>"), so this never
	// fires in practice — still guard against filter-string injection.
	if strings.Contains(runName, "'") {
		return Obs{}, fmt.Errorf("mlflow: run name must not contain a single quote: %q", runName)
	}

	// VERIFY(mlflow-field): default experiment id "0" + run_name filter
	// syntax against the real addon.
	body, err := json.Marshal(map[string]any{
		"experiment_ids": []string{"0"},
		"filter":         fmt.Sprintf("attributes.run_name = '%s'", runName),
		"max_results":    1,
	})
	if err != nil {
		return Obs{}, fmt.Errorf("mlflow: encode search request: %w", err)
	}

	url := strings.TrimRight(m.BaseURL, "/") + "/api/2.0/mlflow/runs/search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Obs{}, fmt.Errorf("mlflow: build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client().Do(req)
	if err != nil {
		return Obs{}, fmt.Errorf("mlflow: search request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Obs{}, fmt.Errorf("mlflow: read search response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return Obs{}, fmt.Errorf("mlflow: search returned %s: %s", resp.Status, snippet)
	}

	var parsed mlflowSearchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Obs{}, fmt.Errorf("mlflow: decode search response: %w", err)
	}
	if len(parsed.Runs) == 0 {
		return Obs{}, nil
	}
	// MLflow's search response carries the latest value per metric key
	// directly in run.data.metrics.
	for _, mm := range parsed.Runs[0].Data.Metrics {
		if mm.Key == metric {
			return Obs{Value: mm.Value, Step: mm.Step, Found: true}, nil
		}
	}
	return Obs{}, nil
}
