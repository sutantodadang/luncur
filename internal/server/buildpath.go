package server

import (
	"fmt"
	"strings"
)

// validBuildPath validates and normalizes a per-app build_path: an optional
// repo-relative subdirectory used as the build context/detection dir
// (monorepo support — one git repo backing several apps). "" is valid and
// means "build the repo root", the pre-existing behavior. Shared by the JSON
// API (handleCreateApp) and the UI (handleUICreateApp) so both enforce the
// same rules.
func validBuildPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if len(p) > 200 {
		return "", fmt.Errorf("build path must be at most 200 characters, got %d", len(p))
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("build path must be relative (no leading '/')")
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		return "", fmt.Errorf("build path must not be a windows-style absolute path")
	}
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("build path must not contain backslashes")
	}
	if strings.HasPrefix(p, "./") {
		return "", fmt.Errorf("build path must not start with './'")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("build path must not contain '..' segments")
		}
	}
	for _, r := range p {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == '-'
		if !ok {
			return "", fmt.Errorf("build path contains invalid character %q", r)
		}
	}
	return strings.TrimSuffix(p, "/"), nil
}
