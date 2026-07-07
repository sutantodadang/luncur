package store

import (
	"path/filepath"
	"testing"
)

func TestCreateModelApp(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "model.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := st.CreateProject("mlp")

	a, err := st.CreateModelApp(p.ID, "gemma", "hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "model" || a.Port != 0 {
		t.Fatalf("app: %+v", a)
	}
	got, err := st.GetApp(p.ID, "gemma")
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelSource != "hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf" || got.Runtime != "auto" {
		t.Fatalf("roundtrip: %+v", got)
	}

	if _, err := st.CreateModelApp(p.ID, "bad", "ftp://x", ""); err == nil {
		t.Fatal("bad source scheme must be rejected")
	}
	if _, err := st.CreateModelApp(p.ID, "bad2", "hf:o/n", "bogus"); err == nil {
		t.Fatal("bad runtime must be rejected")
	}
	// model kind rejects a port via the shared matrix.
	if _, err := st.CreateApp(p.ID, "bad3", 8080, "model", ""); err == nil {
		t.Fatal("model app with a port must be rejected")
	}
}
