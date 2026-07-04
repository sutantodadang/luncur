package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// cannedDoctorServer serves a fixed /v1/doctor JSON response so CLI
// rendering/exit-code behavior can be tested without a real server.Deps.
func cannedDoctorServer(t *testing.T, serverVersion string, checks []map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/doctor", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"server_version": serverVersion,
			"checks":         checks,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// setCLIConfig points the CLI's local config at a fake server without going
// through the login flow (which needs a real /v1/login endpoint).
func setCLIConfig(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := saveConfig(Config{Server: serverURL, Token: "test-token"}); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCommandAllOK(t *testing.T) {
	srv := cannedDoctorServer(t, version, []map[string]string{
		{"name": "database", "status": "ok", "detail": "reachable"},
		{"name": "kubernetes", "status": "ok", "detail": "1 node(s) ready"},
	})
	setCLIConfig(t, srv.URL)

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v (%s)", err, out)
	}
	for _, want := range []string{"CHECK", "STATUS", "DETAIL", "database", "kubernetes", "version"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in output, got %q", want, out)
		}
	}
}

func TestDoctorCommandFailExitsNonZero(t *testing.T) {
	srv := cannedDoctorServer(t, version, []map[string]string{
		{"name": "database", "status": "ok", "detail": "reachable"},
		{"name": "kubernetes", "status": "fail", "detail": "kubernetes is not configured"},
	})
	setCLIConfig(t, srv.URL)

	out, err := run(t, "doctor")
	if err == nil {
		t.Fatalf("want error when a check fails, got none (out=%s)", out)
	}
	if !strings.Contains(out, "kubernetes") {
		t.Fatalf("want failing check printed in table, got %q", out)
	}
}

func TestDoctorCommandWarnOnlySucceeds(t *testing.T) {
	srv := cannedDoctorServer(t, version, []map[string]string{
		{"name": "smtp", "status": "warn", "detail": "not configured — invite emails disabled"},
	})
	setCLIConfig(t, srv.URL)

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("warn-only should succeed: %v (%s)", err, out)
	}
	if !strings.Contains(out, "warn") {
		t.Fatalf("want warn status in output, got %q", out)
	}
}

func TestDoctorCommandVersionMismatch(t *testing.T) {
	srv := cannedDoctorServer(t, "v9.9.9", nil)
	setCLIConfig(t, srv.URL)

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("version mismatch alone should not fail: %v (%s)", err, out)
	}
	if !strings.Contains(out, "v9.9.9") || !strings.Contains(out, "warn") {
		t.Fatalf("want version mismatch warn row, got %q", out)
	}
}

func TestDoctorCommandVersionMatch(t *testing.T) {
	srv := cannedDoctorServer(t, version, nil)
	setCLIConfig(t, srv.URL)

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v (%s)", err, out)
	}
	if !strings.Contains(out, "version") || !strings.Contains(out, "ok") {
		t.Fatalf("want version ok row, got %q", out)
	}
}
