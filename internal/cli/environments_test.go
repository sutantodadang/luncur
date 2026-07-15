package cli

import (
	"strings"
	"testing"
)

// TestEnvsCommands exercises the `luncur envs` group (create + list) end to
// end against a real server, and confirms it is distinct from the `luncur env`
// (env-var) command.
func TestEnvsCommands(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "web"); err != nil {
		t.Fatal(err)
	}

	// Project create seeds develop/staging/production.
	out, err := run(t, "envs", "list", "--project", "web")
	if err != nil {
		t.Fatalf("envs list: %v (%s)", err, out)
	}
	for _, want := range []string{"develop", "staging", "production"} {
		if !strings.Contains(out, want) {
			t.Fatalf("seeded env %q missing from list:\n%s", want, out)
		}
	}

	if out, err = run(t, "envs", "create", "qa", "--project", "web", "--base-branch", "qa"); err != nil {
		t.Fatalf("envs create: %v (%s)", err, out)
	}
	if !strings.Contains(out, "created qa") {
		t.Fatalf("create output = %q", out)
	}

	out, err = run(t, "envs", "list", "--project", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "qa") {
		t.Fatalf("created env not listed:\n%s", out)
	}

	if out, err = run(t, "envs", "set-default", "qa", "--project", "web"); err != nil {
		t.Fatalf("set-default: %v (%s)", err, out)
	}
}

// TestEnvFlagOnAppCommand confirms an app subcommand accepts --env and that
// `envs` and `env` are distinct top-level commands.
func TestEnvFlagOnAppCommand(t *testing.T) {
	var listFound bool
	for _, c := range appCmd().Commands() {
		if c.Name() == "list" {
			listFound = true
			if c.Flags().Lookup("env") == nil {
				t.Fatal("app list missing --env flag")
			}
		}
	}
	if !listFound {
		t.Fatal("app list subcommand not found")
	}

	// env (var) and envs (deployment env) are distinct top-level commands.
	names := map[string]bool{}
	for _, c := range newRoot().Commands() {
		names[c.Name()] = true
	}
	if !names["env"] || !names["envs"] {
		t.Fatalf("want both env and envs commands, got %v", names)
	}
}
