package cli

import (
	"strings"
	"testing"
)

func TestProjectAppEnvCommands(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "project", "create", "web"); err != nil || !strings.Contains(out, "web") {
		t.Fatalf("project create: %v %q", err, out)
	}
	if out, err := run(t, "app", "create", "api", "--project", "web", "--port", "3000"); err != nil || !strings.Contains(out, "api") {
		t.Fatalf("app create: %v %q", err, out)
	}
	if out, err := run(t, "env", "set", "api", "K=v", "--project", "web"); err != nil {
		t.Fatal(err)
	} else if strings.Contains(out, "K=v") || !strings.Contains(out, "set K on api") {
		// "env set" must echo only the key, never the value.
		t.Fatalf("env set output must not echo the value: %q", out)
	}
	if out, _ := run(t, "env", "list", "api", "--project", "web"); !strings.Contains(out, "K=v") {
		t.Fatalf("env list: %q", out)
	}
	if out, _ := run(t, "app", "list", "--project", "web"); !strings.Contains(out, "api") {
		t.Fatalf("app list: %q", out)
	}
	// No kube in test server: deploy must surface the 503 cleanly.
	if _, err := run(t, "deploy", "api", "--project", "web", "--image", "nginx:1"); err == nil || !strings.Contains(err.Error(), "kubernetes_unavailable") {
		t.Fatalf("deploy: want kubernetes_unavailable error, got %v", err)
	}
	// No live deployment exists (deploy above failed before creating one), so
	// scale does not require kube and should succeed.
	if _, err := run(t, "scale", "api", "--project", "web", "--replicas", "2"); err != nil {
		t.Fatalf("scale: %v", err)
	}
	// handleDeleteApp always requires kube, so destroy must surface the same 503.
	if _, err := run(t, "destroy", "api", "--project", "web"); err == nil || !strings.Contains(err.Error(), "kubernetes_unavailable") {
		t.Fatalf("destroy: want kubernetes_unavailable error, got %v", err)
	}
}
