package cli

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=test@example.com", "-c", "user.name=test"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func tarFileContents(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

// In a git checkout, deploy uploads the working tree as it is on disk:
// uncommitted edits and untracked (non-ignored) files included.
func TestPackSourceDeploysWorkingTree(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('v1')\n"), 0o644)
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "v1")
	os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('v2')\n"), 0o644) // the fix the user just made

	r, err := packSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := tarFileContents(t, r)["main.py"]; got != "print('v2')\n" {
		t.Fatalf("deploy uploads main.py = %q, user's working copy is v2 (no warning about uncommitted changes)", got)
	}
}

// Deploying from a subdirectory of a git repo (a monorepo service) still
// honors .gitignore, so ignored secrets like .env stay out of the build context.
func TestPackSourceMonorepoSubdirHonorsGitignore(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "services", "api")
	os.MkdirAll(app, 0o755)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".env\n"), 0o644)
	os.WriteFile(filepath.Join(app, "main.py"), []byte("print(1)\n"), 0o644)
	os.WriteFile(filepath.Join(app, ".env"), []byte("STRIPE_KEY=sk_live_x\n"), 0o644)
	gitT(t, root, "init", "-q")
	gitT(t, root, "add", ".")
	gitT(t, root, "commit", "-qm", "init")

	r, err := packSource(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := tarFileContents(t, r)[".env"]; leaked {
		t.Fatal("git-ignored .env uploaded in the build context when deploying from a repo subdirectory")
	}
}
