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
	for _, sub := range []string{"sources", "logs"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", sub, err)
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
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
