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

// Source is the on-disk store for uploaded build tarballs and build logs,
// rooted at the server's --data-dir. In production the same directory is a
// PVC mounted into both the luncur pod and each Build Job.
type Source struct{ dir string }

func NewSource(dataDir string) (*Source, error) {
	// The Build Job's rootless pod (uid 1000) must traverse these dirs to
	// read tarballs and append to its log file, while the server usually
	// runs as root — world-accessible on purpose. Chmod explicitly because
	// MkdirAll's mode is filtered by the umask and the dirs may pre-exist
	// from an older, stricter version.
	for _, sub := range []string{"sources", "logs"} {
		p := filepath.Join(dataDir, sub)
		if err := os.MkdirAll(p, 0o777); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", sub, err)
		}
		if err := os.Chmod(p, 0o777); err != nil {
			return nil, fmt.Errorf("chmod %s dir: %w", sub, err)
		}
	}
	return &Source{dir: dataDir}, nil
}

func (s *Source) TarballPath(deployID int64) string {
	return filepath.Join(s.dir, "sources", fmt.Sprintf("%d.tar.gz", deployID))
}

func (s *Source) LogPath(deployID int64) string {
	return filepath.Join(s.dir, "logs", fmt.Sprintf("%d.log", deployID))
}

func (s *Source) Save(deployID int64, r io.Reader) (string, error) {
	path := s.TarballPath(deployID)
	// 0644: the rootless builder pod (uid 1000) reads this tarball.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o644); err != nil {
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Source) ReadLog(deployID int64) ([]byte, error) {
	b, err := os.ReadFile(s.LogPath(deployID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}
