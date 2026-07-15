package cli

import (
	"strings"
	"testing"
)

// TestPreviewCommands exercises the `luncur preview` group's CLI wiring
// end to end against a real (in-process) server. testEnv has no kube, so
// `preview create` surfaces the server's kubernetes_unavailable error —
// the same honest, no-cluster-needed check other kube-dependent commands
// rely on (mirrors TestAddonCommands in commands_test.go). `preview ls`
// against a project with none yet still returns a clean (empty) table.
func TestPreviewCommands(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "preview", "ls", "--project", "p")
	if err != nil {
		t.Fatalf("preview ls: %v (%s)", err, out)
	}
	if !strings.Contains(out, "NAME") {
		t.Fatalf("want header, got %q", out)
	}

	_, err = run(t, "preview", "create", "feature/x", "--project", "p")
	if err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("want kubernetes error, got %v", err)
	}

	// preview and previews-adjacent (envs) are distinct top-level commands.
	names := map[string]bool{}
	for _, c := range newRoot().Commands() {
		names[c.Name()] = true
	}
	if !names["preview"] || !names["envs"] {
		t.Fatalf("want both preview and envs commands, got %v", names)
	}
}
