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

// GPUNodeLabel is the key=value label a GPU node carries (kept in sync
// with render.GPUNodeLabelKey/Value; spelled out here so this leaf package
// does not import render).
const GPUNodeLabel = "luncur.dev/gpu=true"

// EnsureK3sAgent installs K3s in agent mode (official script, pinned
// version) joined to serverURL, when k3s is missing on this machine. With
// gpu, the agent self-labels luncur.dev/gpu=true at registration so the
// device plugin DaemonSet and GPU workloads schedule onto it.
func EnsureK3sAgent(r Runner, serverURL, token string, gpu bool) (bool, error) {
	if _, err := r.Run("which", "k3s"); err == nil {
		return false, nil
	}
	exec := ""
	if gpu {
		exec = fmt.Sprintf(" INSTALL_K3S_EXEC=%q", "agent --node-label "+GPUNodeLabel)
	}
	script := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s K3S_URL=%s K3S_TOKEN=%s%s sh -",
		K3sVersion, serverURL, token, exec)
	if out, err := r.Run("sh", "-c", script); err != nil {
		return false, fmt.Errorf("k3s agent install failed: %v\n%s", err, out)
	}
	return true, nil
}

// CheckNVIDIADriver verifies the NVIDIA driver is installed and working by
// running nvidia-smi.
func CheckNVIDIADriver(r Runner) error {
	if _, err := r.Run("nvidia-smi"); err != nil {
		return fmt.Errorf("nvidia driver not found (nvidia-smi failed: %v) — install the NVIDIA driver for this GPU first", err)
	}
	return nil
}

// EnsureNVIDIAToolkit installs nvidia-container-toolkit (which K3s
// auto-detects at agent start, registering the "nvidia" containerd runtime)
// when it is missing. Supports apt-get and dnf hosts; anything else gets an
// instructive error.
func EnsureNVIDIAToolkit(r Runner) (bool, error) {
	if _, err := r.Run("which", "nvidia-ctk"); err == nil {
		return false, nil
	}
	if _, err := r.Run("which", "apt-get"); err == nil {
		script := "curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg && " +
			"curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' > /etc/apt/sources.list.d/nvidia-container-toolkit.list && " +
			"apt-get update && apt-get install -y nvidia-container-toolkit"
		if out, err := r.Run("sh", "-c", script); err != nil {
			return false, fmt.Errorf("nvidia-container-toolkit install failed: %v\n%s", err, out)
		}
		return true, nil
	}
	if _, err := r.Run("which", "dnf"); err == nil {
		script := "curl -s -L https://nvidia.github.io/libnvidia-container/stable/rpm/nvidia-container-toolkit.repo > /etc/yum.repos.d/nvidia-container-toolkit.repo && " +
			"dnf install -y nvidia-container-toolkit"
		if out, err := r.Run("sh", "-c", script); err != nil {
			return false, fmt.Errorf("nvidia-container-toolkit install failed: %v\n%s", err, out)
		}
		return true, nil
	}
	return false, fmt.Errorf("no supported package manager found (apt-get or dnf) — install nvidia-container-toolkit manually, then re-run")
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
