package cli

import "testing"

func TestParseEnvPairs(t *testing.T) {
	m, err := parseEnvPairs([]string{"POSTGRES_PASSWORD=secret", "POSTGRES_DB=vec", "EMPTY="})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["POSTGRES_PASSWORD"] != "secret" || m["POSTGRES_DB"] != "vec" || m["EMPTY"] != "" {
		t.Fatalf("bad map: %v", m)
	}

	// Values may contain '=' (e.g. base64, DSNs).
	m, err = parseEnvPairs([]string{"DSN=postgres://u:p@h/db?x=1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["DSN"] != "postgres://u:p@h/db?x=1" {
		t.Fatalf("value with '=' mangled: %q", m["DSN"])
	}

	for _, bad := range []string{"NOEQUALS", "=novalue"} {
		if _, err := parseEnvPairs([]string{bad}); err == nil {
			t.Fatalf("want error for %q, got nil", bad)
		}
	}
}

func TestDeployEnvFlag(t *testing.T) {
	cmd := deployCmd()
	if cmd.Flags().Lookup("env") == nil {
		t.Fatal("missing --env flag")
	}
	if cmd.Flags().ShorthandLookup("e") == nil {
		t.Fatal("missing -e shorthand")
	}
}
