package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// cannedNodesServer serves a fixed /v1/nodes JSON response so CLI
// rendering can be tested without a real server.Deps.
func cannedNodesServer(t *testing.T, nodes []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestNodeLsRendersTable(t *testing.T) {
	srv := cannedNodesServer(t, []map[string]any{
		{"name": "cp1", "role": "control-plane", "ready": true, "ip": "1.2.3.4", "version": "v1.32.5+k3s1"},
		{"name": "agent1", "role": "agent", "ready": false, "ip": "10.0.0.5", "version": "v1.32.5+k3s1"},
	})
	setCLIConfig(t, srv.URL)

	out, err := run(t, "node", "ls")
	if err != nil {
		t.Fatalf("node ls: %v (%s)", err, out)
	}
	for _, want := range []string{"NAME", "ROLE", "STATUS", "IP", "VERSION",
		"cp1", "control-plane", "Ready", "agent1", "agent", "NotReady", "1.2.3.4", "10.0.0.5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in output, got %q", want, out)
		}
	}
}

func TestNodeJoinCommandRefusesNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux host")
	}
	cmd := nodeCmd()
	cmd.SetArgs([]string{"join-command"})
	if err := cmd.Execute(); err == nil ||
		!strings.Contains(err.Error(), "linux") {
		t.Fatalf("want linux-only error, got %v", err)
	}
}

func TestJoinRefusesNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux host")
	}
	cmd := joinCmd()
	cmd.SetArgs([]string{"https://1.2.3.4:6443", "--token", "tok"})
	if err := cmd.Execute(); err == nil ||
		!strings.Contains(err.Error(), "linux") {
		t.Fatalf("want linux-only error, got %v", err)
	}
}
