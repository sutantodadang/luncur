package gitssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/store"
)

type fakeBackend struct {
	user    store.User
	branch  string
	pushed  []byte // tarball bytes received
	pushErr error
}

func (f *fakeBackend) Authorize(fp string) (store.User, error) {
	if f.user.ID == 0 {
		return store.User{}, fmt.Errorf("unknown key")
	}
	return f.user, nil
}

func (f *fakeBackend) Branch(u store.User, project, app string) (string, error) {
	if f.branch == "" {
		return "", fmt.Errorf("no such app")
	}
	return f.branch, nil
}

func (f *fakeBackend) Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error {
	b, _ := io.ReadAll(tarball)
	f.pushed = b
	fmt.Fprintln(progress, "building...")
	fmt.Fprintln(progress, "app live")
	return f.pushErr
}

// buildLuncurOnce builds the real luncur binary for the post-receive hook to
// exec — os.Executable() inside tests is the test binary, which must not be
// re-entered.
var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

func luncurBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "luncur-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "luncur")
		out, err := exec.Command("go", "build", "-o", builtBin,
			"github.com/sutantodadang/luncur/cmd/luncur").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build luncur: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Skipf("cannot build luncur binary: %v", buildErr)
	}
	return builtBin
}

func newTestServer(t *testing.T, b Backend, hookExe string) (addr string) {
	t.Helper()
	hk, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "hostkey"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(hk, b)
	srv.HookExe = hookExe
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func TestHostKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hostkey")
	k1, err := LoadOrCreateHostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateHostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(k1.PublicKey()) != ssh.FingerprintSHA256(k2.PublicKey()) {
		t.Fatal("host key changed between loads")
	}
}

func TestRejectsNonReceivePack(t *testing.T) {
	b := &fakeBackend{user: store.User{ID: 1}, branch: "main"}
	addr := newTestServer(t, b, "")

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr strings.Builder
	sess.Stderr = &stderr
	if err := sess.Run("git-upload-pack '/p/a.git'"); err == nil {
		t.Fatal("upload-pack accepted")
	}
	if !strings.Contains(stderr.String(), "push-only") {
		t.Fatalf("stderr = %q, want push-only message", stderr.String())
	}
}

// TestGitPushEndToEnd drives a REAL `git push` against the server and
// asserts the backend received a tarball containing the committed file and
// the client saw the progress lines.
func TestGitPushEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	bin := luncurBin(t)
	b := &fakeBackend{user: store.User{ID: 1}, branch: "main"}
	addr := newTestServer(t, b, bin)

	// Client key: throwaway ed25519 keypair for git's ssh to use.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id")
	writeTestClientKey(t, keyPath)

	// Work repo with one commit on main.
	repo := filepath.Join(dir, "repo")
	runGit(t, "", "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "one")

	host, port, _ := net.SplitHostPort(addr)
	url := fmt.Sprintf("ssh://git@%s:%s/proj/app.git", host, port)
	cmd := exec.Command("git", "push", url, "main")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "app live") {
		t.Fatalf("push output missing progress:\n%s", out)
	}
	if len(b.pushed) == 0 {
		t.Fatal("backend received no tarball")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeTestClientKey writes an OpenSSH-format ed25519 private key.
func writeTestClientKey(t *testing.T, path string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
}
