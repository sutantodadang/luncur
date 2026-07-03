// Package gitssh implements luncur's push-only git-over-SSH receiver.
package gitssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/store"
)

// Backend is what the SSH layer needs from the rest of luncur.
type Backend interface {
	// Authorize resolves a public-key fingerprint to a user.
	Authorize(fingerprint string) (store.User, error)
	// Branch validates that u may push to project/app (membership +
	// existence) and returns the branch that triggers a deploy. The error
	// text is shown to the git client.
	Branch(u store.User, project, app string) (string, error)
	// Push consumes the archived source tarball, blocks until the deploy
	// finishes, and writes human progress lines to progress. The error text
	// is shown to the client after the log.
	Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error
}

type Server struct {
	cfg     *ssh.ServerConfig
	backend Backend

	// HookExe is the binary the post-receive hook execs. Defaults to
	// os.Executable() (the running luncur binary); tests point it at a
	// freshly built cmd/luncur.
	HookExe string

	mu        sync.Mutex
	listeners map[net.Listener]struct{}
	closed    bool
}

const fpKey = "luncur-fingerprint"

func New(hostKey ssh.Signer, backend Backend) *Server {
	s := &Server{backend: backend, listeners: map[net.Listener]struct{}{}}
	s.cfg = &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			fp := ssh.FingerprintSHA256(key)
			if _, err := backend.Authorize(fp); err != nil {
				return nil, fmt.Errorf("unknown key")
			}
			return &ssh.Permissions{Extensions: map[string]string{fpKey: fp}}, nil
		},
	}
	s.cfg.AddHostKey(hostKey)
	return s
}

func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("server closed")
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for l := range s.listeners {
		l.Close()
	}
	return nil
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	fp := conn.Permissions.Extensions[fpKey]
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only sessions supported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs, fp)
	}
}

// receivePackRe extracts project/app from the exec command line, tolerating
// optional quotes and leading slash: git-receive-pack '/proj/app.git'
var receivePackRe = regexp.MustCompile(`^git-receive-pack '?/?([a-z0-9-]+)/([a-z0-9-]+)\.git'?$`)

func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, fp string) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			// payload: uint32 length + command string
			if len(req.Payload) < 4 {
				req.Reply(false, nil)
				return
			}
			cmdline := string(req.Payload[4:])
			m := receivePackRe.FindStringSubmatch(strings.TrimSpace(cmdline))
			if m == nil {
				req.Reply(true, nil)
				fmt.Fprintf(ch.Stderr(), "luncur is push-only: use git push (repo path must be /<project>/<app>.git)\n")
				exitSession(ch, 1)
				return
			}
			req.Reply(true, nil)
			code := s.runReceive(ch, fp, m[1], m[2])
			exitSession(ch, code)
			return
		case "env", "shell", "pty-req":
			// env is harmless; shells/ptys are not offered.
			req.Reply(req.Type == "env", nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// runReceive authorizes and hands off to the git plumbing in receive.go.
func (s *Server) runReceive(ch ssh.Channel, fp, project, app string) int {
	u, err := s.backend.Authorize(fp)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "access denied\n")
		return 1
	}
	branch, err := s.backend.Branch(u, project, app)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "%v\n", err)
		return 1
	}
	if err := s.receive(ch, u, project, app, branch); err != nil {
		fmt.Fprintf(ch.Stderr(), "push failed: %v\n", err)
		return 1
	}
	return 0
}

func exitSession(ch ssh.Channel, code int) {
	payload := make([]byte, 4)
	payload[3] = byte(code)
	ch.SendRequest("exit-status", false, payload)
}
