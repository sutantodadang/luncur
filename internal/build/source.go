// Package build renders luncur's in-cluster build pipeline: the BuildKit
// Job that turns app source into an image, the registry/system infra it
// pushes to, and the on-disk source+log store shared with the Job.
package build

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BuilderGID is the gid of the rootless BuildKit user baked into the
// builder image (moby/buildkit:rootless user `user`, uid/gid 1000). The
// server chowns shared PVC paths to this group so the builder pod can
// append logs and read tarballs without world-writable modes.
const BuilderGID = 1000

// Source is the on-disk store for uploaded build tarballs and build logs,
// rooted at the server's --data-dir. In production the same directory is a
// PVC mounted into both the luncur pod and each Build Job.
type Source struct{ dir string }

func NewSource(dataDir string) (*Source, error) {
	// The Build Job's rootless pod (uid/gid 1000) must traverse these dirs
	// to read tarballs and append to its log file, while the server usually
	// runs as root — share via group 1000, not world-writable modes. Setgid
	// so builder-created files inherit the group. Chown/Chmod best-effort:
	// they fail on non-Linux dev machines, where no builder pod exists.
	// dataDir itself is the builder pod's subPath mount root — it must be
	// group-traversable too, or uid 1000 is stopped before ever reaching
	// the subdirs.
	if err := os.MkdirAll(dataDir, 0o770); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	_ = os.Chown(dataDir, -1, BuilderGID)
	_ = os.Chmod(dataDir, 0o2770)
	for _, sub := range []string{"sources", "logs"} {
		p := filepath.Join(dataDir, sub)
		if err := os.MkdirAll(p, 0o770); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", sub, err)
		}
		_ = os.Chown(p, -1, BuilderGID)
		_ = os.Chmod(p, 0o2770)
	}
	return &Source{dir: dataDir}, nil
}

func (s *Source) TarballPath(deployID string) string {
	return filepath.Join(s.dir, "sources", fmt.Sprintf("%s.tar.gz", deployID))
}

func (s *Source) LogPath(deployID string) string {
	return filepath.Join(s.dir, "logs", fmt.Sprintf("%s.log", deployID))
}

func (s *Source) Save(deployID string, r io.Reader) (string, error) {
	path := s.TarballPath(deployID)
	// Group-readable for the rootless builder pod (gid 1000); chown
	// best-effort for non-Linux dev machines.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_ = f.Chown(-1, BuilderGID)
	if err := f.Chmod(0o640); err != nil {
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Source) ReadLog(deployID string) ([]byte, error) {
	b, err := os.ReadFile(s.LogPath(deployID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}
