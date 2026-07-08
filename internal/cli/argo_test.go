package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// argoInstallFake serves the admin-only /v1/system/argo-install endpoint,
// capturing whether it was called (pipelineFake's pattern).
type argoInstallFake struct {
	srv     *httptest.Server
	called  bool
	version string
}

func newArgoInstallFake(t *testing.T, version string) *argoInstallFake {
	t.Helper()
	f := &argoInstallFake{version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/system/argo-install", func(w http.ResponseWriter, r *http.Request) {
		f.called = true
		json.NewEncoder(w).Encode(map[string]any{"installed": true, "version": f.version})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// TestArgoInstallPrintsVersionAndReminder covers `luncur argo install`: it
// calls the endpoint and prints the accepted version plus the
// pipeline_engine reminder.
func TestArgoInstallPrintsVersionAndReminder(t *testing.T) {
	f := newArgoInstallFake(t, "v3.6.2")
	setCLIConfig(t, f.srv.URL)

	out, err := run(t, "argo", "install")
	if err != nil {
		t.Fatalf("argo install: %v (%s)", err, out)
	}
	if !f.called {
		t.Fatal("argo-install endpoint was never called")
	}
	if !strings.Contains(out, "v3.6.2") {
		t.Fatalf("output must contain the installed version, got %q", out)
	}
	if !strings.Contains(out, "pipeline_engine=argo") {
		t.Fatalf("output must remind the operator how to switch engines, got %q", out)
	}
}
