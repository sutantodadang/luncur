package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// packSource builds a tar.gz of dir for a source-based deploy. If dir is
// inside a git work tree (its root or any subdirectory, e.g. one service of
// a monorepo), it packs the working tree's files that git doesn't ignore —
// tracked files with their current, possibly uncommitted contents, plus
// untracked files not matched by .gitignore — so what deploys is what's on
// disk, and ignored secrets like .env never enter the build context.
// Otherwise it walks the tree itself, skipping .git, node_modules, and
// .luncur.
func packSource(dir string) (io.Reader, error) {
	files, inGit, err := gitSourceFiles(dir)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if inGit {
		for _, rel := range files {
			if err := addTarFile(tw, dir, filepath.FromSlash(rel)); err != nil {
				return nil, err
			}
		}
	} else {
		skip := map[string]bool{".git": true, "node_modules": true, ".luncur": true}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == dir {
				return nil
			}
			if d.IsDir() {
				if skip[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			return addTarFile(tw, dir, rel)
		})
		if err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// gitSourceFiles lists the files under dir that git doesn't ignore, as
// slash-separated paths relative to dir. inGit is false (with no error)
// when dir isn't inside a git work tree or git isn't installed.
func gitSourceFiles(dir string) (files []string, inGit bool, err error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false, nil
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil, false, nil
	}
	// Run from dir, ls-files lists only dir's subtree, relative to dir.
	cmd := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, false, fmt.Errorf("git ls-files: %w: %s", err, ee.Stderr)
		}
		return nil, false, fmt.Errorf("git ls-files: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		files = append(files, p)
	}
	return files, true, nil
}

// addTarFile writes dir/rel into tw: regular files with their contents,
// symlinks as link entries (target unchanged, as git archive stores them).
// Anything else — a directory (e.g. a submodule), another special file, or
// a tracked file deleted from the working tree — is skipped.
func addTarFile(tw *tar.Writer, dir, rel string) error {
	path := filepath.Join(dir, rel)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		return tw.WriteHeader(hdr)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = filepath.ToSlash(rel)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}
