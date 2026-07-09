package cli

import "testing"

// buildTestArchive returns a valid backup archive, reusing restore_test.go's
// fixture builder (validArchive already produces luncur.db + luncur.key +
// manifest.json in the shape restoreArchive expects).
func buildTestArchive(t *testing.T) string {
	t.Helper()
	return validArchive(t)
}

// corruptTestArchive returns a well-formed tar.gz whose luncur.db member is
// not a real SQLite file, so restoreArchive succeeds (it just copies bytes)
// but the subsequent integrity check fails.
func corruptTestArchive(t *testing.T) string {
	t.Helper()
	return makeArchive(t, map[string][]byte{
		"luncur.db":     []byte("not a real sqlite database"),
		"luncur.key":    []byte("keybytes-32-keybytes-32-keybyte!"),
		"manifest.json": []byte(`{"created_at":"2026-07-04T00:00:00Z","members":["luncur.db"]}`),
	})
}

func TestVerifyArchive(t *testing.T) {
	archive := buildTestArchive(t) // same fixture helper style as restore_test.go
	rep, err := verifyArchive(archive)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Integrity != "ok" {
		t.Fatalf("integrity = %q, want ok", rep.Integrity)
	}
	if rep.Tables == 0 {
		t.Fatal("expected schema tables in restored DB")
	}
	if !rep.SealerKey {
		t.Fatal("sealer key missing from archive")
	}
}

func TestVerifyArchiveCorrupt(t *testing.T) {
	if _, err := verifyArchive(corruptTestArchive(t)); err == nil {
		t.Fatal("corrupt archive must fail verification")
	}
}
