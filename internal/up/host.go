package up

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

// ExecRunner shells out for real; `luncur up` uses it, tests fake it.
type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

const (
	K3sVersion       = "v1.32.5+k3s1" // pinned; bumped deliberately, never floated
	K3sKubeconfig    = "/etc/rancher/k3s/k3s.yaml"
	RegistriesPath   = "/etc/rancher/k3s/registries.yaml"
	RegistryNodePort = 30500
	NodeTokenPath    = "/var/lib/rancher/k3s/server/node-token"
)

// EnsureK3s installs K3s (official script, pinned version) when missing.
func EnsureK3s(r Runner) (bool, error) {
	if _, err := r.Run("which", "k3s"); err == nil {
		return false, nil
	}
	script := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s sh -", K3sVersion)
	if out, err := r.Run("sh", "-c", script); err != nil {
		return false, fmt.Errorf("k3s install failed: %v\n%s", err, out)
	}
	return true, nil
}

// EnsureK3sAgent installs K3s in agent mode (official script, pinned
// version) joined to serverURL, when k3s is missing on this machine.
func EnsureK3sAgent(r Runner, serverURL, token string) (bool, error) {
	if _, err := r.Run("which", "k3s"); err == nil {
		return false, nil
	}
	script := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s K3S_URL=%s K3S_TOKEN=%s sh -",
		K3sVersion, serverURL, token)
	if out, err := r.Run("sh", "-c", script); err != nil {
		return false, fmt.Errorf("k3s agent install failed: %v\n%s", err, out)
	}
	return true, nil
}

// RegistriesYAML maps the in-cluster registry hostname to the localhost
// NodePort — containerd on the node cannot resolve cluster-DNS names.
func RegistriesYAML() string {
	return fmt.Sprintf(`mirrors:
  "registry.luncur-system:5000":
    endpoint:
      - "http://127.0.0.1:%d"
`, RegistryNodePort)
}

func WriteRegistriesYAML(path string) (bool, error) {
	want := []byte(RegistriesYAML())
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, want) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return false, err
	}
	return true, nil
}
