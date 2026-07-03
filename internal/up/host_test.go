package up

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct{ cmds [][]string }

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, append([]string{name}, args...))
	if name == "which" { // k3s missing
		return nil, fmt.Errorf("not found")
	}
	return nil, nil
}

func TestEnsureK3sInstallsWhenMissing(t *testing.T) {
	f := &fakeRunner{}
	installed, err := EnsureK3s(f)
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("expected install")
	}
	joined := fmt.Sprint(f.cmds)
	if !strings.Contains(joined, "get.k3s.io") || !strings.Contains(joined, K3sVersion) {
		t.Fatalf("install command wrong: %v", f.cmds)
	}
}

func TestWriteRegistriesYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registries.yaml")
	changed, err := WriteRegistriesYAML(p)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first write must report changed")
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{"registry.luncur-system:5000", "http://127.0.0.1:30500"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("registries.yaml missing %q:\n%s", want, b)
		}
	}
	changed, err = WriteRegistriesYAML(p)
	if err != nil || changed {
		t.Fatalf("second write: changed=%v err=%v, want false nil", changed, err)
	}
}
