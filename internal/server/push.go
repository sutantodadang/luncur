package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// PushBackend adapts the server's deploy pipeline to gitssh.Backend.
type PushBackend struct{ s *server }

func (b *PushBackend) Authorize(fingerprint string) (store.User, error) {
	return b.s.st.UserForSSHFingerprint(fingerprint)
}

// Branch validates access and returns the deploy branch: the app's
// configured git branch when set, else "main".
func (b *PushBackend) Branch(u store.User, project, app string) (string, error) {
	p, err := b.s.st.GetProject(project)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no such project %q", project)
	}
	if err != nil {
		return "", fmt.Errorf("internal error")
	}
	if u.Role != "admin" {
		ok, err := b.s.st.IsMember(p.ID, u.ID)
		if err != nil || !ok {
			return "", fmt.Errorf("not a member of project %q", project)
		}
	}
	a, err := b.s.st.GetApp(p.ID, app)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no such app %q in project %q", app, project)
	}
	if err != nil {
		return "", fmt.Errorf("internal error")
	}
	if a.Ejected {
		return "", errAppEjected
	}
	if a.GitBranch != "" {
		return a.GitBranch, nil
	}
	return "main", nil
}

// Push saves the tarball as a new deployment and runs the build
// synchronously, tailing the build log into progress while it runs.
func (b *PushBackend) Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error {
	s := b.s
	if s.kube == nil {
		return fmt.Errorf("kubernetes unavailable on the server")
	}
	if s.src == nil {
		return fmt.Errorf("server has no data dir configured")
	}
	p, err := s.st.GetProject(project)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	a, err := s.st.GetApp(p.ID, app)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}

	d, err := s.st.CreateDeployment(a.ID, "building", "", u.ID)
	if err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	if _, err := s.src.Save(d.ID, tarball); err != nil {
		s.st.SetDeploymentStatus(d.ID, "failed")
		return fmt.Errorf("save source: %w", err)
	}

	fmt.Fprintf(progress, "-----> deploy %s building\n", d.ID)

	// Tail the build log into the pusher's terminal while runBuild runs.
	done := make(chan struct{})
	go tailFile(done, s.src.LogPath(d.ID), progress)

	err = s.runBuild(ctx, p, a, d)
	close(done)

	if err != nil {
		return fmt.Errorf("deploy %s failed — see: luncur logs %s --project %s --deploy %s", d.ID, app, project, d.ID)
	}
	fmt.Fprintf(progress, "-----> app live: http://%s\n", hostFor(a.Name, s.externalIP))
	return nil
}

// tailFile streams appended lines of path to w until done closes, then
// drains whatever remains. Missing file = keep polling (the build Job
// creates it).
func tailFile(done <-chan struct{}, path string, w io.Writer) {
	var off int64
	flush := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return
		}
		b, _ := io.ReadAll(f)
		if len(b) == 0 {
			return
		}
		off += int64(len(b))
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			fmt.Fprintln(w, line)
		}
	}
	for {
		select {
		case <-done:
			flush()
			return
		case <-time.After(500 * time.Millisecond):
			flush()
		}
	}
}

// NewWithBackend builds the HTTP handler plus the push backend bound to the
// same server instance, so `luncur serve` can wire both from one Deps. The
// third return starts the server's background loops — cert issuance/renewal
// (no-op unless the provider is builtin and kube is configured), the
// scheduled-backup loop, the weekly registry GC sweep, a one-shot startup
// reconciliation of any deployment a previous server process left stranded
// in 'building'/'deploying' (see reconcile.go), and the metrics monitor
// sampler — callers that don't need them may discard it.
func NewWithBackend(d Deps) (http.Handler, *PushBackend, func(ctx context.Context)) {
	s := newServer(d)
	start := func(ctx context.Context) {
		s.StartCerts(ctx)
		go s.StartBackups(ctx)
		go s.StartRegistryGC(ctx)
		go s.reconcileUnfinished(ctx)
		go s.StartMonitor(ctx)
		go s.StartGPUWatch(ctx)
		s.StartGPUIdleLoop(ctx)
		go s.startSweepLoop(ctx)
		// A previous process may have applied only the base Ingress (e.g.
		// `luncur up` re-running); re-assert panel_domain here so a custom
		// domain set before restart isn't silently dropped until the next
		// settings write or daily cert sweep.
		go func() {
			actx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.applyPanelIngress(actx); err != nil {
				log.Printf("apply panel ingress at startup: %v", err)
			}
		}()
	}
	return s.handler(), &PushBackend{s: s}, start
}
