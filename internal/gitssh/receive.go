package gitssh

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/store"
)

// receive runs git-receive-pack into a throwaway bare repo. A post-receive
// hook (this same binary, hidden command `_push-hook`) relays the pushed
// refs to us over a unix socket; we archive the deploy branch, run the
// backend push synchronously, and stream progress lines back through the
// hook so the git client prints them as "remote: ...".
func (s *Server) receive(ch ssh.Channel, u store.User, project, app, branch string) error {
	tmp, err := os.MkdirTemp("", "luncur-push-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	repo := filepath.Join(tmp, "repo.git")

	if out, err := exec.Command("git", "init", "--bare", "--quiet", repo).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %v\n%s", err, out)
	}

	sock := filepath.Join(tmp, "hook.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer l.Close()

	hookErr := make(chan error, 1)
	go func() { hookErr <- s.serveHook(l, u, project, app, branch, repo) }()

	exe := s.HookExe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	hook := fmt.Sprintf("#!/bin/sh\nexec %q _push-hook\n", exe)
	hooksDir := filepath.Join(repo, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(hook), 0o755); err != nil {
		return err
	}

	cmd := exec.Command("git-receive-pack", repo)
	cmd.Env = append(os.Environ(), "LUNCUR_PUSH_SOCK="+sock)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git-receive-pack: %w", err)
	}

	select {
	case err := <-hookErr:
		return err
	case <-time.After(time.Second):
		// No hook connection: nothing was pushed (e.g. everything up to
		// date). Not an error.
		return nil
	}
}

// serveHook accepts the single post-receive connection. Protocol: hook
// sends the ref-update lines ("<old> <new> <refname>\n") followed by a
// blank line; we stream progress lines back; final line "__luncur_exit__ N".
func (s *Server) serveHook(l net.Listener, u store.User, project, app, branch, repo string) error {
	conn, err := l.Accept()
	if err != nil {
		return nil // receive-pack finished without invoking the hook
	}
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	var pushedSHA string
	want := "refs/heads/" + branch
	var refs []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		refs = append(refs, line)
		parts := strings.Fields(line)
		if len(parts) == 3 && parts[2] == want {
			pushedSHA = parts[1]
		}
	}

	fail := func(format string, a ...any) error {
		fmt.Fprintf(conn, format+"\n", a...)
		fmt.Fprintln(conn, "__luncur_exit__ 1")
		return fmt.Errorf(format, a...)
	}

	if pushedSHA == "" || pushedSHA == strings.Repeat("0", 40) {
		return fail("nothing deployed: push the %q branch (got: %s)", branch, strings.Join(refs, ", "))
	}

	// Archive the pushed commit as tar.gz — same format tarball deploys use.
	fmt.Fprintf(conn, "-----> archiving %s\n", pushedSHA[:8])
	archive := exec.Command("git", "-C", repo, "archive", "--format=tar.gz", pushedSHA)
	tarball, err := archive.StdoutPipe()
	if err != nil {
		return fail("archive: %v", err)
	}
	var archErr strings.Builder
	archive.Stderr = &archErr
	if err := archive.Start(); err != nil {
		return fail("archive: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	pushErr := s.backend.Push(ctx, u, project, app, tarball, conn)
	if werr := archive.Wait(); werr != nil && pushErr == nil {
		pushErr = fmt.Errorf("git archive: %v\n%s", werr, archErr.String())
	}
	if pushErr != nil {
		return fail("BUILD FAILED: %v", pushErr)
	}
	fmt.Fprintln(conn, "__luncur_exit__ 0")
	return nil
}
