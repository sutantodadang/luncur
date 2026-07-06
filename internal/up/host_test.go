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

type presentRunner struct{ cmds [][]string }

func (p *presentRunner) Run(name string, args ...string) ([]byte, error) {
	p.cmds = append(p.cmds, append([]string{name}, args...))
	if name == "which" { // k3s already installed
		return []byte("/usr/local/bin/k3s"), nil
	}
	return nil, nil
}

func TestEnsureK3sAgentSkippedWhenPresent(t *testing.T) {
	p := &presentRunner{}
	installed, err := EnsureK3sAgent(p, "https://1.2.3.4:6443", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Fatal("expected no install when k3s already present")
	}
}

func TestEnsureK3sAgentInstallsWhenMissing(t *testing.T) {
	f := &fakeRunner{}
	installed, err := EnsureK3sAgent(f, "https://1.2.3.4:6443", "sekret")
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("expected install")
	}
	joined := fmt.Sprint(f.cmds)
	for _, want := range []string{"get.k3s.io", K3sVersion, "K3S_URL=https://1.2.3.4:6443", "K3S_TOKEN=sekret"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("install command missing %q: %v", want, f.cmds)
		}
	}
}

func TestEnsureK3sAgentInstallErrorPropagates(t *testing.T) {
	f := &failingRunner{}
	installed, err := EnsureK3sAgent(f, "https://1.2.3.4:6443", "sekret")
	if err == nil {
		t.Fatal("expected error")
	}
	if installed {
		t.Fatal("installed should be false on error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want to contain install output", err)
	}
}

type failingRunner struct{}

func (failingRunner) Run(name string, args ...string) ([]byte, error) {
	if name == "which" {
		return nil, fmt.Errorf("not found")
	}
	return []byte("boom"), fmt.Errorf("exit status 1")
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
