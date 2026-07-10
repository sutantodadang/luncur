package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleForwardApp tunnels one TCP connection to <app>.<namespace>:<port>.
// The CLI sends Upgrade: luncur-tunnel/1; after the 101 the connection is a
// raw byte pipe (one local connection = one request, kubectl-style). Only
// the app's own Service port may be dialed — no arbitrary host/port (SSRF).
func (s *server) handleForwardApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if a.Kind != "" && a.Kind != "web" {
		writeError(w, http.StatusConflict, "no_service", "only web apps have a Service to forward to")
		return
	}
	if q := r.URL.Query().Get("port"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n != a.Port {
			writeError(w, http.StatusBadRequest, "bad_port", fmt.Sprintf("app only serves port %d", a.Port))
			return
		}
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "luncur-tunnel/1") {
		writeError(w, http.StatusBadRequest, "bad_upgrade", "expected Upgrade: luncur-tunnel/1")
		return
	}

	// The app Service listens on 80 and targets the container's app port
	// (render.go) — dialing a.Port on the ClusterIP would hang: kube-proxy
	// silently drops non-service ports.
	addr := fmt.Sprintf("%s.%s:80", a.Name, p.Namespace)
	backend, err := s.fwdDial(r.Context(), "tcp", addr)
	if err != nil {
		log.Printf("forward dial %s: %v", addr, err)
		writeError(w, http.StatusBadGateway, "dial_failed", "could not reach the app's service")
		return
	}

	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		backend.Close()
		log.Printf("forward hijack: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "connection cannot be tunneled")
		return
	}
	defer conn.Close()
	defer backend.Close()

	// A buffered-write error surfaces at the checked Flush below.
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: luncur-tunnel/1\r\nConnection: Upgrade\r\n\r\n")
	if err := buf.Flush(); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, backend); done <- struct{}{} }()
	<-done
}
